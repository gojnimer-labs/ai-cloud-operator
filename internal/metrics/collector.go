/*
Copyright 2026 gojnimer-labs.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package metrics collects per-workload usage samples (currently network
// bytes in/out) for periodic batch reporting to Convex — see
// convexclient.Client.ReportMetrics and ai-cloud-v2's
// convex/metrics/mutations.ts#recordBatch. Deliberately its own package,
// not folded into internal/capacity: capacity.Tracker answers "does this
// operator have headroom," a local admission decision computed from static
// Deployment requests; this package answers "how much has each workload
// actually used," a live counter read from the kubelet, reported outward
// for a dashboard chart. Different questions, different data sources, no
// reason to share code beyond both ultimately listing this operator's own
// managed objects.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gojnimer-labs/ai-cloud-operator/internal/labels"
)

// +kubebuilder:rbac:groups="",resources=nodes/proxy,verbs=get

// workloadNameLabel must stay in sync with internal/controller's labelName
// constant — same convention internal/podexec already follows, for the same
// reason: every object the reconciler creates (Deployment, and
// transitively its Pods) carries it, so it's how a Pod is traced back to
// its owning Workload's Kubernetes name without the reconciler needing to
// expose anything new.
const workloadNameLabel = "app.kubernetes.io/name"

// Metric name constants — see convex/schema.ts's workloadMetrics table doc
// comment for why these are free-form dotted strings rather than a fixed
// enum: a future metric is just a new constant here, no schema change on
// either side.
const (
	MetricNetworkRxBytes = "network.rxBytes"
	MetricNetworkTxBytes = "network.txBytes"
)

// Sample is one usage measurement for a single workload, ready to hand to
// convexclient.Client.ReportMetrics.
type Sample struct {
	// Name is the workload's Kubernetes CR name (== its
	// app.kubernetes.io/name label value), the same identity
	// convexclient.Client.ReportLifecycle already reports by — Convex
	// resolves it back to a workload row via (operatorId, name).
	Name      string
	Metric    string
	Value     float64
	SampledAt time.Time
}

// Collector reads live per-pod network counters from each node's kubelet
// stats/summary endpoint, for every Pod this operator manages.
type Collector struct {
	client    client.Client
	clientset kubernetes.Interface
	namespace string
}

// NewCollector returns a Collector scoped to namespace, using c for listing
// this operator's own managed Nodes/Pods (the manager's cached client, same
// as internal/capacity.Tracker) and cfg to build a raw clientset for the
// kubelet stats/summary proxy call — controller-runtime's client.Client has
// no generic support for arbitrary subresource proxy requests, the same
// reason internal/podexec builds its own clientset from the identical
// *rest.Config.
func NewCollector(c client.Client, cfg *rest.Config, namespace string) (*Collector, error) {
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes clientset: %w", err)
	}
	return &Collector{client: c, clientset: clientset, namespace: namespace}, nil
}

// nodeStatsSummary/podStats/podReference/networkStats are the small subset
// of the kubelet's /stats/summary response shape (see
// k8s.io/kubelet/pkg/apis/stats/v1alpha1.Summary) this package actually
// reads — hand-declared rather than importing that package, which would
// pull in a large, mostly-unused API surface for three fields.
type nodeStatsSummary struct {
	Node *nodeStats `json:"node,omitempty"`
	Pods []podStats `json:"pods"`
}

// nodeStats is the kubelet's whole-node reading — CPU/Memory share the same
// shape as a pod's own per-pod reading below, so cpuStats/memoryStats are
// reused for both.
type nodeStats struct {
	CPU    *cpuStats    `json:"cpu,omitempty"`
	Memory *memoryStats `json:"memory,omitempty"`
}

// UsageNanoCores is a pointer for the same reason RxBytes/TxBytes below are:
// the kubelet omits it when no reading is available yet.
type cpuStats struct {
	UsageNanoCores *uint64 `json:"usageNanoCores,omitempty"`
}

// WorkingSetBytes, deliberately not UsageBytes: UsageBytes includes
// evictable page cache and overstates real memory pressure — confirmed
// against a live cluster, where summed UsageBytes came out ~35Gi against
// `kubectl top nodes`' ~18Gi, while summed WorkingSetBytes reconciled at
// ~19.3Gi. Never decode UsageBytes for a usage figure.
type memoryStats struct {
	WorkingSetBytes *uint64 `json:"workingSetBytes,omitempty"`
}

type podStats struct {
	PodRef  podReference  `json:"podRef"`
	CPU     *cpuStats     `json:"cpu,omitempty"`
	Memory  *memoryStats  `json:"memory,omitempty"`
	Network *networkStats `json:"network,omitempty"`
}

type podReference struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// RxBytes/TxBytes are pointers because the kubelet omits them entirely for
// a pod whose network stats aren't available yet (e.g. still starting) —
// nil here means "no reading," not "zero usage."
type networkStats struct {
	RxBytes *uint64 `json:"rxBytes,omitempty"`
	TxBytes *uint64 `json:"txBytes,omitempty"`
}

type podKey struct {
	namespace string
	name      string
}

// nodeStatsTimeout bounds each individual node's stats/summary round trip in
// CollectUsage. Unlike Collect (its own 5-minute ticker), CollectUsage is
// called from every 30s heartbeat tick (see
// convexclient.Runnable.heartbeatOnce) — one unreachable node must never be
// able to stall a heartbeat by more than this long.
const nodeStatsTimeout = 3 * time.Second

// UsageSnapshot is one live, kubelet-sourced usage reading — see
// CollectUsage. Distinct from capacity.Snapshot: that struct is
// requests-vs-allocatable and gates scheduling; this one is live
// usage-vs-allocatable, display-only, and never read for gating.
type UsageSnapshot struct {
	// ClusterUsedMilliCPU/ClusterUsedMemoryBytes sum every reachable Node's
	// own .node.cpu.usageNanoCores/.node.memory.workingSetBytes — the whole
	// cluster, not just nodes hosting a managed pod, matching
	// capacity.Tracker's own all-Nodes Allocatable* sum.
	ClusterUsedMilliCPU    int64
	ClusterUsedMemoryBytes int64
	// ManagedUsedMilliCPU/ManagedUsedMemoryBytes sum live
	// .pods[].cpu.usageNanoCores/.pods[].memory.workingSetBytes for exactly
	// the Pods capacity.Tracker's own UsedMilliCPU/UsedMemoryBytes counts
	// *requests* for (same namespace + labels.ManagedBy scope) — read from
	// the same per-node stats/summary payload Collect's network samples
	// already come from; no second fetch.
	ManagedUsedMilliCPU    int64
	ManagedUsedMemoryBytes int64
	// NodesReporting/NodesTotal let a caller tell a full reading from a
	// degraded one (some nodes unreachable) — ClusterUsed*/ManagedUsed*
	// alone would otherwise silently understate usage with no signal that
	// they're partial.
	NodesReporting int
	NodesTotal     int
}

// Collect returns one Sample per (managed pod, metric) currently read —
// network.rxBytes/network.txBytes for every Pod this operator manages that
// has already been scheduled and has a network reading available. Visits
// only the nodes that actually host a managed pod, one stats/summary call
// per such node (not per pod) — cAdvisor's summary endpoint already returns
// every pod on that node in one response. A single unreachable/erroring
// node is logged and skipped rather than failing the whole collection —
// consistent with this being best-effort telemetry (see
// convex/metrics/mutations.ts#recordBatch's own doc comment), not
// correctness-critical.
func (c *Collector) Collect(ctx context.Context) ([]Sample, error) {
	log := logf.FromContext(ctx)

	var pods corev1.PodList
	if err := c.client.List(ctx, &pods, client.InNamespace(c.namespace), client.MatchingLabels{labels.ManagedBy: labels.ManagedByValue}); err != nil {
		return nil, fmt.Errorf("listing managed pods: %w", err)
	}

	wantByNode := make(map[string]map[podKey]string)
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == "" {
			continue
		}
		workloadName := pod.Labels[workloadNameLabel]
		if workloadName == "" {
			continue
		}
		if wantByNode[pod.Spec.NodeName] == nil {
			wantByNode[pod.Spec.NodeName] = make(map[podKey]string)
		}
		wantByNode[pod.Spec.NodeName][podKey{namespace: pod.Namespace, name: pod.Name}] = workloadName
	}

	now := time.Now()
	var samples []Sample
	for nodeName, want := range wantByNode {
		summary, err := c.fetchNodeSummary(ctx, nodeName)
		if err != nil {
			log.Error(err, "fetching kubelet stats summary", "node", nodeName)
			continue
		}
		for _, ps := range summary.Pods {
			workloadName, ok := want[podKey{namespace: ps.PodRef.Namespace, name: ps.PodRef.Name}]
			if !ok || ps.Network == nil {
				continue
			}
			if ps.Network.RxBytes != nil {
				samples = append(samples, Sample{Name: workloadName, Metric: MetricNetworkRxBytes, Value: float64(*ps.Network.RxBytes), SampledAt: now})
			}
			if ps.Network.TxBytes != nil {
				samples = append(samples, Sample{Name: workloadName, Metric: MetricNetworkTxBytes, Value: float64(*ps.Network.TxBytes), SampledAt: now})
			}
		}
	}
	return samples, nil
}

// CollectUsage returns a live UsageSnapshot by querying every Node's kubelet
// stats/summary endpoint once — cluster-wide, unlike Collect (which only
// visits nodes hosting a managed pod), since ClusterUsed* needs every
// node's own .node reading regardless of what's scheduled there. Each
// node's round trip is bounded by nodeStatsTimeout; a node that errors or
// times out is logged and its contribution simply omitted from the sum
// (reflected in NodesReporting vs NodesTotal) — the same best-effort
// posture Collect already has toward an unreachable node, stricter here
// since this runs on every 30s heartbeat tick rather than the 5-minute
// metrics ticker. Only List failures for Nodes/Pods themselves (cached
// reads, not network calls) return a non-nil error.
func (c *Collector) CollectUsage(ctx context.Context) (UsageSnapshot, error) {
	log := logf.FromContext(ctx)

	var nodes corev1.NodeList
	if err := c.client.List(ctx, &nodes); err != nil {
		return UsageSnapshot{}, fmt.Errorf("listing nodes: %w", err)
	}

	var pods corev1.PodList
	if err := c.client.List(ctx, &pods, client.InNamespace(c.namespace), client.MatchingLabels{labels.ManagedBy: labels.ManagedByValue}); err != nil {
		return UsageSnapshot{}, fmt.Errorf("listing managed pods: %w", err)
	}
	managed := make(map[podKey]struct{}, len(pods.Items))
	for _, pod := range pods.Items {
		managed[podKey{namespace: pod.Namespace, name: pod.Name}] = struct{}{}
	}

	snap := UsageSnapshot{NodesTotal: len(nodes.Items)}
	for _, n := range nodes.Items {
		nodeCtx, cancel := context.WithTimeout(ctx, nodeStatsTimeout)
		summary, err := c.fetchNodeSummary(nodeCtx, n.Name)
		cancel()
		if err != nil {
			log.Error(err, "fetching kubelet stats summary for live usage", "node", n.Name)
			continue
		}
		snap.NodesReporting++

		if summary.Node != nil {
			if summary.Node.CPU != nil && summary.Node.CPU.UsageNanoCores != nil {
				snap.ClusterUsedMilliCPU += int64(*summary.Node.CPU.UsageNanoCores / 1_000_000)
			}
			if summary.Node.Memory != nil && summary.Node.Memory.WorkingSetBytes != nil {
				snap.ClusterUsedMemoryBytes += int64(*summary.Node.Memory.WorkingSetBytes)
			}
		}
		for _, ps := range summary.Pods {
			if _, ok := managed[podKey{namespace: ps.PodRef.Namespace, name: ps.PodRef.Name}]; !ok {
				continue
			}
			if ps.CPU != nil && ps.CPU.UsageNanoCores != nil {
				snap.ManagedUsedMilliCPU += int64(*ps.CPU.UsageNanoCores / 1_000_000)
			}
			if ps.Memory != nil && ps.Memory.WorkingSetBytes != nil {
				snap.ManagedUsedMemoryBytes += int64(*ps.Memory.WorkingSetBytes)
			}
		}
	}
	return snap, nil
}

func (c *Collector) fetchNodeSummary(ctx context.Context, nodeName string) (*nodeStatsSummary, error) {
	raw, err := c.clientset.CoreV1().RESTClient().Get().
		Resource("nodes").
		Name(nodeName).
		SubResource("proxy").
		Suffix("stats/summary").
		Do(ctx).
		Raw()
	if err != nil {
		return nil, fmt.Errorf("requesting stats summary: %w", err)
	}
	var summary nodeStatsSummary
	if err := json.Unmarshal(raw, &summary); err != nil {
		return nil, fmt.Errorf("decoding stats summary: %w", err)
	}
	return &summary, nil
}

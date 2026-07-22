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

package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gojnimer-labs/ai-cloud-operator/internal/labels"
)

const (
	testNamespace = "ai-cloud-workloads"
	testNode      = "node-1"
)

// fakeKubeletServer stands in for a real apiserver's /nodes/{name}/proxy
// passthrough to a kubelet's own stats/summary endpoint — client-go's
// RESTClient only cares that rest.Config.Host serves the exact path it
// requests, not that it's genuinely proxied through a real apiserver, so
// pointing it straight at this httptest server exercises the real request/
// decode path without needing envtest (which has no kubelet to proxy to at
// all).
func fakeKubeletServer(t *testing.T, byNode map[string]nodeStatsSummary) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for node, summary := range byNode {
		mux.HandleFunc(fmt.Sprintf("/api/v1/nodes/%s/proxy/stats/summary", node), func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(summary)
		})
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func newTestCollector(t *testing.T, server *httptest.Server, pods ...*corev1.Pod) *Collector {
	t.Helper()
	objs := make([]client.Object, len(pods))
	for i, p := range pods {
		objs[i] = p
	}
	return newTestCollectorWithObjects(t, server, objs...)
}

// newTestCollectorWithObjects is newTestCollector generalized to also seed
// Nodes — needed for CollectUsage, which (unlike Collect) lists all Nodes
// cluster-wide rather than only discovering them via a managed pod's
// spec.nodeName.
func newTestCollectorWithObjects(t *testing.T, server *httptest.Server, objs ...client.Object) *Collector {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		builder = builder.WithObjects(o)
	}
	fakeClient := builder.Build()

	collector, err := NewCollector(fakeClient, &rest.Config{Host: server.URL}, testNamespace)
	if err != nil {
		t.Fatalf("building collector: %v", err)
	}
	return collector
}

func fakeNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func managedPod(name, nodeName, workloadName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels: map[string]string{
				labels.ManagedBy:  labels.ManagedByValue,
				workloadNameLabel: workloadName,
			},
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
		},
	}
}

func uint64Ptr(v uint64) *uint64 { return &v }

func TestCollectReadsNetworkCountersForManagedPods(t *testing.T) {
	pod := managedPod("firefox-abc12-xyz", testNode, "firefox-abc12")
	server := fakeKubeletServer(t, map[string]nodeStatsSummary{
		testNode: {
			Pods: []podStats{
				{
					PodRef:  podReference{Name: "firefox-abc12-xyz", Namespace: testNamespace},
					Network: &networkStats{RxBytes: uint64Ptr(1024), TxBytes: uint64Ptr(512)},
				},
				// A pod on the same node this operator doesn't manage —
				// must not leak into the results.
				{
					PodRef:  podReference{Name: "someone-elses-pod", Namespace: testNamespace},
					Network: &networkStats{RxBytes: uint64Ptr(999), TxBytes: uint64Ptr(999)},
				},
			},
		},
	})
	collector := newTestCollector(t, server, pod)

	samples, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d: %+v", len(samples), samples)
	}
	byMetric := map[string]float64{}
	for _, s := range samples {
		if s.Name != "firefox-abc12" {
			t.Fatalf("expected sample for firefox-abc12, got %q", s.Name)
		}
		byMetric[s.Metric] = s.Value
	}
	if byMetric[MetricNetworkRxBytes] != 1024 {
		t.Fatalf("expected rxBytes 1024, got %v", byMetric[MetricNetworkRxBytes])
	}
	if byMetric[MetricNetworkTxBytes] != 512 {
		t.Fatalf("expected txBytes 512, got %v", byMetric[MetricNetworkTxBytes])
	}
}

func TestCollectSkipsUnscheduledPods(t *testing.T) {
	pod := managedPod("firefox-pending", "", "firefox-pending")
	server := fakeKubeletServer(t, map[string]nodeStatsSummary{})
	collector := newTestCollector(t, server, pod)

	samples, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("expected no samples for an unscheduled pod, got %+v", samples)
	}
}

func TestCollectSkipsPodsMissingWorkloadNameLabel(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-workload-label",
			Namespace: testNamespace,
			Labels:    map[string]string{labels.ManagedBy: labels.ManagedByValue},
		},
		Spec: corev1.PodSpec{NodeName: testNode},
	}
	server := fakeKubeletServer(t, map[string]nodeStatsSummary{
		testNode: {Pods: []podStats{{
			PodRef:  podReference{Name: "no-workload-label", Namespace: testNamespace},
			Network: &networkStats{RxBytes: uint64Ptr(1), TxBytes: uint64Ptr(1)},
		}}},
	})
	collector := newTestCollector(t, server, pod)

	samples, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("expected no samples for a pod with no workload-name label, got %+v", samples)
	}
}

func TestCollectContinuesPastAnUnreachableNode(t *testing.T) {
	healthy := managedPod("firefox-ok", "node-healthy", "firefox-ok")
	broken := managedPod("firefox-stuck", "node-unreachable", "firefox-stuck")
	server := fakeKubeletServer(t, map[string]nodeStatsSummary{
		"node-healthy": {Pods: []podStats{{
			PodRef:  podReference{Name: "firefox-ok", Namespace: testNamespace},
			Network: &networkStats{RxBytes: uint64Ptr(10), TxBytes: uint64Ptr(20)},
		}}},
		// node-unreachable deliberately has no handler registered — the
		// fake server 404s it, simulating a node whose kubelet can't be
		// reached.
	})
	collector := newTestCollector(t, server, healthy, broken)

	samples, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples from the healthy node despite the unreachable one, got %+v", samples)
	}
	for _, s := range samples {
		if s.Name != "firefox-ok" {
			t.Fatalf("expected only firefox-ok's samples, got %q", s.Name)
		}
	}
}

// TestCollectUsageSumsClusterWideAcrossAllNodes confirms ClusterUsedMilliCPU/
// ClusterUsedMemoryBytes count every Node's own reading, including a node
// hosting no managed pod at all — the key difference from Collect, which
// only ever visits nodes a managed pod happens to be scheduled on.
func TestCollectUsageSumsClusterWideAcrossAllNodes(t *testing.T) {
	pod := managedPod("firefox-abc12-xyz", "node-with-pod", "firefox-abc12")
	server := fakeKubeletServer(t, map[string]nodeStatsSummary{
		"node-with-pod": {
			Node: &nodeStats{CPU: &cpuStats{UsageNanoCores: uint64Ptr(500_000_000)}, Memory: &memoryStats{WorkingSetBytes: uint64Ptr(1024)}},
		},
		"node-empty": {
			Node: &nodeStats{CPU: &cpuStats{UsageNanoCores: uint64Ptr(200_000_000)}, Memory: &memoryStats{WorkingSetBytes: uint64Ptr(2048)}},
		},
	})
	collector := newTestCollectorWithObjects(t, server, pod, fakeNode("node-with-pod"), fakeNode("node-empty"))

	snap, err := collector.CollectUsage(context.Background())
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if snap.ClusterUsedMilliCPU != 700 {
		t.Fatalf("expected ClusterUsedMilliCPU to sum both nodes' readings (500+200), got %d", snap.ClusterUsedMilliCPU)
	}
	if snap.ClusterUsedMemoryBytes != 3072 {
		t.Fatalf("expected ClusterUsedMemoryBytes to sum both nodes' readings (1024+2048), got %d", snap.ClusterUsedMemoryBytes)
	}
	if snap.NodesReporting != 2 || snap.NodesTotal != 2 {
		t.Fatalf("expected 2/2 nodes reporting, got %d/%d", snap.NodesReporting, snap.NodesTotal)
	}
}

// TestCollectUsageFiltersManagedUsageByNamespaceAndLabel confirms
// ManagedUsedMilliCPU/ManagedUsedMemoryBytes only ever count pods in this
// operator's own namespace+managed-by scope, ignoring another pod on the
// same node's kubelet response — the same isolation Collect already
// guarantees for network bytes, now extended to CPU/memory.
func TestCollectUsageFiltersManagedUsageByNamespaceAndLabel(t *testing.T) {
	pod := managedPod("firefox-abc12-xyz", testNode, "firefox-abc12")
	server := fakeKubeletServer(t, map[string]nodeStatsSummary{
		testNode: {
			Node: &nodeStats{CPU: &cpuStats{UsageNanoCores: uint64Ptr(1_000_000_000)}, Memory: &memoryStats{WorkingSetBytes: uint64Ptr(4096)}},
			Pods: []podStats{
				{
					PodRef: podReference{Name: "firefox-abc12-xyz", Namespace: testNamespace},
					CPU:    &cpuStats{UsageNanoCores: uint64Ptr(300_000_000)},
					Memory: &memoryStats{WorkingSetBytes: uint64Ptr(1500)},
				},
				// Unmanaged pod on the same node — must not leak into
				// ManagedUsed*.
				{
					PodRef: podReference{Name: "someone-elses-pod", Namespace: testNamespace},
					CPU:    &cpuStats{UsageNanoCores: uint64Ptr(900_000_000)},
					Memory: &memoryStats{WorkingSetBytes: uint64Ptr(9999)},
				},
			},
		},
	})
	collector := newTestCollectorWithObjects(t, server, pod, fakeNode(testNode))

	snap, err := collector.CollectUsage(context.Background())
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if snap.ManagedUsedMilliCPU != 300 {
		t.Fatalf("expected ManagedUsedMilliCPU to count only the managed pod (300), got %d", snap.ManagedUsedMilliCPU)
	}
	if snap.ManagedUsedMemoryBytes != 1500 {
		t.Fatalf("expected ManagedUsedMemoryBytes to count only the managed pod (1500), got %d", snap.ManagedUsedMemoryBytes)
	}
	// The node-level reading is cluster-wide and includes both pods —
	// unaffected by the managed-pod filter, which only applies to
	// ManagedUsed*.
	if snap.ClusterUsedMilliCPU != 1000 {
		t.Fatalf("expected ClusterUsedMilliCPU to reflect the whole node's reading (1000), got %d", snap.ClusterUsedMilliCPU)
	}
}

// TestCollectUsageSkipsUnreachableNodeAndReflectsInNodesReporting confirms a
// kubelet round-trip failure for one node is best-effort — logged and
// skipped, not a hard error for the whole call — while NodesReporting vs
// NodesTotal makes the partial reading visible to callers rather than
// silently presenting an undercount as authoritative.
func TestCollectUsageSkipsUnreachableNodeAndReflectsInNodesReporting(t *testing.T) {
	server := fakeKubeletServer(t, map[string]nodeStatsSummary{
		"node-healthy": {
			Node: &nodeStats{CPU: &cpuStats{UsageNanoCores: uint64Ptr(500_000_000)}, Memory: &memoryStats{WorkingSetBytes: uint64Ptr(1024)}},
		},
		// node-unreachable deliberately has no handler registered — the fake
		// server 404s it, simulating a node whose kubelet can't be reached.
	})
	collector := newTestCollectorWithObjects(t, server, fakeNode("node-healthy"), fakeNode("node-unreachable"))

	snap, err := collector.CollectUsage(context.Background())
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if snap.NodesTotal != 2 {
		t.Fatalf("expected NodesTotal to count both nodes, got %d", snap.NodesTotal)
	}
	if snap.NodesReporting != 1 {
		t.Fatalf("expected NodesReporting to count only the healthy node, got %d", snap.NodesReporting)
	}
	if snap.ClusterUsedMilliCPU != 500 {
		t.Fatalf("expected ClusterUsedMilliCPU to reflect only the healthy node's reading, got %d", snap.ClusterUsedMilliCPU)
	}
}

// TestCollectUsageReadsWorkingSetBytesNotUsageBytes is the concrete guard
// against decoding the wrong memory field: memoryStats intentionally has no
// UsageBytes field at all, so a fixture where the kubelet also reports a
// (deliberately different, much larger) usageBytes value alongside
// workingSetBytes must never influence the result — confirming a future
// accidental field-name swap would be caught here rather than silently
// reintroducing the overstated-memory bug this feature exists to fix.
func TestCollectUsageReadsWorkingSetBytesNotUsageBytes(t *testing.T) {
	server := fakeKubeletServer(t, map[string]nodeStatsSummary{
		testNode: {
			Node: &nodeStats{Memory: &memoryStats{WorkingSetBytes: uint64Ptr(2000)}},
		},
	})
	collector := newTestCollectorWithObjects(t, server, fakeNode(testNode))

	snap, err := collector.CollectUsage(context.Background())
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if snap.ClusterUsedMemoryBytes != 2000 {
		t.Fatalf("expected ClusterUsedMemoryBytes to equal workingSetBytes (2000), got %d", snap.ClusterUsedMemoryBytes)
	}
}

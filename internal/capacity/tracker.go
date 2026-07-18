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

// Package capacity estimates whether this operator has headroom to claim
// another workload — purely a local, operator-side decision. Convex never
// gates a claim on capacity (see the architecture plan's Context section):
// the claim mutation is already a race (first caller wins), so an
// overloaded operator simply not calling it is sufficient for another
// operator's heartbeat to pick up the slack. Convex only optionally
// receives a capacity report for admin fleet-visibility, never for gating.
//
// The model is requests-vs-allocatable: sum of resource *requests* this
// operator's own managed Deployments declare (already in the manager's warm
// cache — Owns(&appsv1.Deployment{}) in internal/controller — so no new API
// load) versus summed Node .status.allocatable across the cluster (new
// cluster-scoped RBAC; Nodes aren't watched by anything else in this
// operator). This is dynamic — it changes automatically as workloads come
// and go — without needing live-utilization/metrics-server sampling, and it
// matches how the Kubernetes scheduler itself reasons about fit (by
// requests, not live usage), which avoids the flapping risk a noisy live
// signal would add to an admission decision.
package capacity

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gojnimer-labs/ai-cloud-operator/internal/labels"
)

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Snapshot is one point-in-time capacity reading.
type Snapshot struct {
	AllocatableMilliCPU    int64
	AllocatableMemoryBytes int64
	UsedMilliCPU           int64
	UsedMemoryBytes        int64
}

// Fits reports whether estMilliCPU/estMemoryBytes still fit within s's
// remaining headroom (allocatable minus used).
func (s Snapshot) Fits(estMilliCPU, estMemoryBytes int64) bool {
	return s.AllocatableMilliCPU-s.UsedMilliCPU >= estMilliCPU &&
		s.AllocatableMemoryBytes-s.UsedMemoryBytes >= estMemoryBytes
}

// Tracker computes Snapshots on demand from a controller-runtime client —
// expected to be the manager's own cached client (see cmd/main.go), so
// repeated Snapshot calls cost no extra API traffic beyond the first List of
// each type (which also warms the cache's informer for that type).
type Tracker struct {
	client    client.Client
	namespace string
}

// NewTracker returns a Tracker that sums Deployments in namespace against
// cluster-wide Node allocatable capacity, using c.
func NewTracker(c client.Client, namespace string) *Tracker {
	return &Tracker{client: c, namespace: namespace}
}

// Start implements manager.Runnable purely to tie this Tracker's lifecycle to
// the manager, via the same mgr.Add seam already used for
// convexclient.Runnable/api.Server — Snapshot itself is pull-based, called
// synchronously from the heartbeat loop's self-gate, not driven by this
// method.
func (t *Tracker) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Snapshot sums Node.Status.Allocatable across every Node in the cluster —
// v1: unconditionally, including cordoned/tainted nodes, a known coarseness
// acceptable for a coarse self-gate — against the sum of resource requests
// declared by every Deployment this operator manages (labels.ManagedBy) in
// t.namespace, multiplying each container's Requests by *Spec.Replicas (nil
// treated as 1, matching the Deployment API's own default) so a stopped
// (replicas: 0) workload counts as zero immediately.
func (t *Tracker) Snapshot(ctx context.Context) (Snapshot, error) {
	var nodes corev1.NodeList
	if err := t.client.List(ctx, &nodes); err != nil {
		return Snapshot{}, fmt.Errorf("listing nodes: %w", err)
	}

	var snap Snapshot
	for _, n := range nodes.Items {
		snap.AllocatableMilliCPU += n.Status.Allocatable.Cpu().MilliValue()
		snap.AllocatableMemoryBytes += n.Status.Allocatable.Memory().Value()
	}

	var deployments appsv1.DeploymentList
	if err := t.client.List(ctx, &deployments, client.InNamespace(t.namespace), client.MatchingLabels{labels.ManagedBy: labels.ManagedByValue}); err != nil {
		return Snapshot{}, fmt.Errorf("listing deployments: %w", err)
	}
	for _, d := range deployments.Items {
		replicas := int64(1)
		if d.Spec.Replicas != nil {
			replicas = int64(*d.Spec.Replicas)
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			snap.UsedMilliCPU += c.Resources.Requests.Cpu().MilliValue() * replicas
			snap.UsedMemoryBytes += c.Resources.Requests.Memory().Value() * replicas
		}
	}
	return snap, nil
}

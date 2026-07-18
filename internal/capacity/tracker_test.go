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

package capacity

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gojnimer-labs/ai-cloud-operator/internal/labels"
)

const testNamespace = "workloads"

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func node(name, cpu, memory string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(memory),
			},
		},
	}
}

func deployment(name string, replicas *int32, managed bool, cpu, memory string) *appsv1.Deployment {
	lbls := map[string]string{}
	if managed {
		lbls[labels.ManagedBy] = labels.ManagedByValue
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: lbls},
		Spec: appsv1.DeploymentSpec{
			Replicas: replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "example.com/image:latest",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(cpu),
									corev1.ResourceMemory: resource.MustParse(memory),
								},
							},
						},
					},
				},
			},
		},
	}
}

func int32Ptr(v int32) *int32 { return &v }

func TestSnapshotSumsNodeAllocatable(t *testing.T) {
	c := newFakeClient(t,
		node("node-1", "2", "4Gi"),
		node("node-2", "4", "8Gi"),
	)
	tracker := NewTracker(c, testNamespace)

	snap, err := tracker.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.AllocatableMilliCPU != 6000 {
		t.Errorf("expected 6000 milliCPU allocatable, got %d", snap.AllocatableMilliCPU)
	}
	wantMemory := int64(12 * 1024 * 1024 * 1024)
	if snap.AllocatableMemoryBytes != wantMemory {
		t.Errorf("expected %d memory bytes allocatable, got %d", wantMemory, snap.AllocatableMemoryBytes)
	}
}

func TestSnapshotSumsManagedDeploymentsOnlyMultipliedByReplicas(t *testing.T) {
	c := newFakeClient(t,
		deployment("managed", int32Ptr(3), true, "100m", "128Mi"),
		deployment("unmanaged", int32Ptr(3), false, "500m", "1Gi"),
	)
	tracker := NewTracker(c, testNamespace)

	snap, err := tracker.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// Only the managed Deployment counts, at 100m * 3 replicas = 300m.
	if snap.UsedMilliCPU != 300 {
		t.Errorf("expected 300 milliCPU used (managed only, x3 replicas), got %d", snap.UsedMilliCPU)
	}
	wantMemory := int64(128 * 1024 * 1024 * 3)
	if snap.UsedMemoryBytes != wantMemory {
		t.Errorf("expected %d memory bytes used, got %d", wantMemory, snap.UsedMemoryBytes)
	}
}

func TestSnapshotTreatsNilReplicasAsOne(t *testing.T) {
	c := newFakeClient(t, deployment("no-replicas-set", nil, true, "250m", "256Mi"))
	tracker := NewTracker(c, testNamespace)

	snap, err := tracker.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.UsedMilliCPU != 250 {
		t.Errorf("expected 250 milliCPU (nil Replicas treated as 1), got %d", snap.UsedMilliCPU)
	}
}

func TestSnapshotTreatsZeroReplicasAsZeroUsage(t *testing.T) {
	c := newFakeClient(t, deployment("stopped", int32Ptr(0), true, "250m", "256Mi"))
	tracker := NewTracker(c, testNamespace)

	snap, err := tracker.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.UsedMilliCPU != 0 || snap.UsedMemoryBytes != 0 {
		t.Errorf("expected a stopped (replicas: 0) workload to count as zero, got milliCPU=%d memoryBytes=%d", snap.UsedMilliCPU, snap.UsedMemoryBytes)
	}
}

func TestSnapshotIgnoresDeploymentsInOtherNamespaces(t *testing.T) {
	other := deployment("elsewhere", int32Ptr(1), true, "500m", "512Mi")
	other.Namespace = "some-other-namespace"
	c := newFakeClient(t, other)
	tracker := NewTracker(c, testNamespace)

	snap, err := tracker.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.UsedMilliCPU != 0 {
		t.Errorf("expected deployments outside t.namespace to be ignored, got milliCPU=%d", snap.UsedMilliCPU)
	}
}

func TestFits(t *testing.T) {
	snap := Snapshot{AllocatableMilliCPU: 1000, AllocatableMemoryBytes: 1024, UsedMilliCPU: 800, UsedMemoryBytes: 512}

	if !snap.Fits(200, 512) {
		t.Error("expected exact remaining headroom to fit")
	}
	if snap.Fits(201, 512) {
		t.Error("expected a request exceeding remaining CPU headroom to not fit")
	}
	if snap.Fits(200, 513) {
		t.Error("expected a request exceeding remaining memory headroom to not fit")
	}
}

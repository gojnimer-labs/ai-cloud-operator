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

package controller

import (
	"testing"

	appsv1alpha1 "github.com/gojnimer-labs/ai-cloud-operator/api/v1alpha1"
)

// TestDesiredReplicaCount is the unit-level proof that Suspended always
// forces 0 replicas regardless of Spec.Replicas, and that unsuspended
// behavior is unchanged from before Suspended existed (nil Replicas falls
// back to defaultReplicas, an explicit value is used as-is) — see
// reconcileDeployment and Reconcile's own desiredReplicas calc, both of
// which now call this single shared helper instead of duplicating the
// pattern independently.
func TestDesiredReplicaCount(t *testing.T) {
	three := int32(3)
	zero := int32(0)

	cases := []struct {
		name string
		spec appsv1alpha1.WorkloadSpec
		want int32
	}{
		{"unsuspended, nil replicas defaults", appsv1alpha1.WorkloadSpec{}, defaultReplicas},
		{"unsuspended, explicit replicas", appsv1alpha1.WorkloadSpec{Replicas: &three}, 3},
		{"unsuspended, explicit zero replicas", appsv1alpha1.WorkloadSpec{Replicas: &zero}, 0},
		{"suspended, nil replicas forces 0", appsv1alpha1.WorkloadSpec{Suspended: true}, 0},
		{"suspended, explicit replicas still forces 0", appsv1alpha1.WorkloadSpec{Suspended: true, Replicas: &three}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workload := &appsv1alpha1.Workload{Spec: tc.spec}
			if got := desiredReplicaCount(workload); got != tc.want {
				t.Fatalf("desiredReplicaCount() = %d, want %d", got, tc.want)
			}
		})
	}
}

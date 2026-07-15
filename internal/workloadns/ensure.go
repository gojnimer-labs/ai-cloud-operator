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

// Package workloadns ensures the single, fixed Kubernetes namespace every
// workload this operator manages gets deployed into (WORKLOAD_NAMESPACE)
// actually exists — an install-time config value, not something Convex
// chooses or sends per-request (see internal/api.Server).
package workloadns

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups="",resources=namespaces,verbs=create

// EnsureNamespace creates the Namespace named name if it doesn't already
// exist. A blind Create (rather than Get-then-create) deliberately avoids
// any dependency on the manager's informer cache — this runs before
// mgr.Start(), when reads through a cached client would hang, but writes go
// straight to the API server and are always safe.
func EnsureNamespace(ctx context.Context, c client.Client, name string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating workload namespace %q: %w", name, err)
	}
	return nil
}

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

package workloadns

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testNamespaceName = "ai-cloud-workloads"

func newFakeClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

func TestEnsureNamespaceCreatesWhenMissing(t *testing.T) {
	c := newFakeClient(t)

	if err := EnsureNamespace(context.Background(), c, testNamespaceName); err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}

	var ns corev1.Namespace
	if err := c.Get(context.Background(), client.ObjectKey{Name: testNamespaceName}, &ns); err != nil {
		t.Fatalf("expected namespace to be created: %v", err)
	}
}

// TestEnsureNamespaceSetsPodSecurityLabelsWhenMissing is the concrete proof
// that a freshly-created workload namespace is pinned to "baseline" — see
// podSecurityLabels' doc comment for why "restricted" (a plausible
// cluster-wide default on a hardened distro) would otherwise silently
// prevent every browser/desktop template's Pods from ever reaching Ready.
func TestEnsureNamespaceSetsPodSecurityLabelsWhenMissing(t *testing.T) {
	c := newFakeClient(t)

	if err := EnsureNamespace(context.Background(), c, testNamespaceName); err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}

	var ns corev1.Namespace
	if err := c.Get(context.Background(), client.ObjectKey{Name: testNamespaceName}, &ns); err != nil {
		t.Fatalf("expected namespace to be created: %v", err)
	}
	for key, want := range podSecurityLabels {
		if got := ns.Labels[key]; got != want {
			t.Fatalf("expected label %q=%q, got %q", key, want, got)
		}
	}
}

func TestEnsureNamespaceNoOpsWhenAlreadyExists(t *testing.T) {
	c := newFakeClient(t)
	existing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespaceName}}
	if err := c.Create(context.Background(), existing); err != nil {
		t.Fatalf("seeding namespace: %v", err)
	}

	if err := EnsureNamespace(context.Background(), c, testNamespaceName); err != nil {
		t.Fatalf("expected no error for an already-existing namespace, got: %v", err)
	}
}

// TestEnsureNamespaceLabelsExistingNamespace is the concrete proof of the
// out-of-band/pre-upgrade case: a namespace this operator (or a human)
// already created before podSecurityLabels existed, with no PodSecurity
// labels of its own, gets them patched on rather than being left as-is —
// otherwise every existing install would stay stuck on whatever the
// cluster's default PodSecurity level is, exactly the bug this whole
// change fixes.
func TestEnsureNamespaceLabelsExistingNamespace(t *testing.T) {
	c := newFakeClient(t)
	existing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{"kubernetes.io/metadata.name": testNamespaceName},
		Name:   testNamespaceName,
	}}
	if err := c.Create(context.Background(), existing); err != nil {
		t.Fatalf("seeding namespace: %v", err)
	}

	if err := EnsureNamespace(context.Background(), c, testNamespaceName); err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}

	var ns corev1.Namespace
	if err := c.Get(context.Background(), client.ObjectKey{Name: testNamespaceName}, &ns); err != nil {
		t.Fatalf("getting namespace: %v", err)
	}
	for key, want := range podSecurityLabels {
		if got := ns.Labels[key]; got != want {
			t.Fatalf("expected label %q=%q, got %q", key, want, got)
		}
	}
	// The merge patch must add labels, not replace the whole map — an
	// unrelated pre-existing label should survive untouched.
	if got := ns.Labels["kubernetes.io/metadata.name"]; got != testNamespaceName {
		t.Fatalf("expected pre-existing label to survive the patch, got %q", got)
	}
}

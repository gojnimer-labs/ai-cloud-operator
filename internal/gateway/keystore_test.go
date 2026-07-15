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

package gateway

import (
	"bytes"
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newFakeClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

// TestLoadOrGenerateCreatesKeyOnFirstUse exercises the empty-cluster path: no
// Secret exists yet, so LoadOrGenerate must mint a fresh key and persist it.
func TestLoadOrGenerateCreatesKeyOnFirstUse(t *testing.T) {
	c := newFakeClient(t)
	store := NewKeyStore(c, "operator-ns")

	key, err := store.LoadOrGenerate(context.Background())
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	if len(key) != signingKeyBytes {
		t.Fatalf("expected a %d-byte key, got %d", signingKeyBytes, len(key))
	}

	var secret corev1.Secret
	if err := c.Get(context.Background(), client.ObjectKey{Name: KeySecretName, Namespace: "operator-ns"}, &secret); err != nil {
		t.Fatalf("expected key to be persisted: %v", err)
	}
	if !bytes.Equal(secret.Data[keySigningKey], key) {
		t.Fatalf("persisted key does not match returned key")
	}
}

// TestLoadOrGenerateReusesPersistedKey guards against silently rotating the
// signing key (and invalidating every outstanding session cookie) on every
// restart.
func TestLoadOrGenerateReusesPersistedKey(t *testing.T) {
	c := newFakeClient(t)
	store := NewKeyStore(c, "operator-ns")

	first, err := store.LoadOrGenerate(context.Background())
	if err != nil {
		t.Fatalf("first LoadOrGenerate: %v", err)
	}

	second, err := store.LoadOrGenerate(context.Background())
	if err != nil {
		t.Fatalf("second LoadOrGenerate: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Fatalf("expected the same key across calls, got two different keys")
	}
}

// TestLoadOrGeneratePrefersExistingSecret covers the case where the Secret
// was already created by someone else (another replica's earlier boot, or a
// human) before this call runs — the existing value must win rather than
// being overwritten by a freshly generated one.
func TestLoadOrGeneratePrefersExistingSecret(t *testing.T) {
	c := newFakeClient(t)
	store := NewKeyStore(c, "operator-ns")

	winningKey := bytes.Repeat([]byte{0x42}, signingKeyBytes)
	if err := c.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: KeySecretName, Namespace: "operator-ns"},
		Data:       map[string][]byte{keySigningKey: winningKey},
	}); err != nil {
		t.Fatalf("seeding winning secret: %v", err)
	}

	got, err := store.LoadOrGenerate(context.Background())
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	if !bytes.Equal(got, winningKey) {
		t.Fatalf("expected the already-persisted key to win, got a different key")
	}
}

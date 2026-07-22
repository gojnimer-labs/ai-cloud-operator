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

package tokenstore

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newFakeStore(t *testing.T) *Store {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	return New(c, "operator-ns")
}

func TestLoadMissingSecretReturnsNotFound(t *testing.T) {
	store := newFakeStore(t)

	tokens, ok, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false when no secret exists, got tokens=%+v", tokens)
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	store := newFakeStore(t)
	ctx := context.Background()

	want := Tokens{HeartbeatToken: "heartbeat-1", DeployToken: "deploy-1", CatalogHash: "hash-1", OperatorVersion: "v1.2.3", TagsFingerprint: "set:gpu"}
	if err := store.Save(ctx, want); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, ok, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true after save")
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSaveTwiceUpdatesExistingSecret(t *testing.T) {
	store := newFakeStore(t)
	ctx := context.Background()

	if err := store.Save(ctx, Tokens{HeartbeatToken: "h1", DeployToken: "d1", CatalogHash: "hash-1", OperatorVersion: "v1.0.0", TagsFingerprint: "set:gpu"}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := store.Save(ctx, Tokens{HeartbeatToken: "h2", DeployToken: "d2", CatalogHash: "hash-2", OperatorVersion: "v2.0.0", TagsFingerprint: "unset"}); err != nil {
		t.Fatalf("second save: %v", err)
	}

	got, ok, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true")
	}
	want := Tokens{HeartbeatToken: "h2", DeployToken: "d2", CatalogHash: "hash-2", OperatorVersion: "v2.0.0", TagsFingerprint: "unset"}
	if got != want {
		t.Fatalf("got %+v, want %+v (rotation should overwrite, not accumulate)", got, want)
	}
}

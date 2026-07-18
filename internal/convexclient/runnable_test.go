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

package convexclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gojnimer-labs/ai-cloud-operator/internal/tokenstore"
)

func newFakeTokenStore(t *testing.T) *tokenstore.Store {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	return tokenstore.New(c, "operator-ns")
}

// newFakeEnrollmentWatcher returns a watcher over a fake client, plus that
// client so tests can mutate the Secret afterward to simulate a rotation. No
// Secret is seeded — Current() returns an error until one is created, same
// as a cluster where ai-cloud-operator-env hasn't been created yet.
func newFakeEnrollmentWatcher(t *testing.T) (*EnrollmentSecretWatcher, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	return NewEnrollmentSecretWatcher(c, "operator-ns"), c
}

func putEnrollmentSecret(t *testing.T, c client.Client, value string) {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: EnrollmentSecretName, Namespace: "operator-ns"},
		Data:       map[string][]byte{enrollmentSecretKey: []byte(value)},
	}
	if err := c.Create(context.Background(), secret); err != nil {
		t.Fatalf("seeding enrollment secret: %v", err)
	}
}

// TestLoadOrRegisterFallsBackWhenNoPersistedToken exercises the empty-store
// path: no Secret exists yet, so the Runnable must call Register and persist
// the result before its first heartbeat.
func TestLoadOrRegisterFallsBackWhenNoPersistedToken(t *testing.T) {
	var registerCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathOperatorsRegister:
			registerCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(registerResponse{HeartbeatToken: testHeartbeatToken, DeployToken: testDeployTokenValue})
		default:
			t.Fatalf("unexpected call to %s before any token exists", r.URL.Path)
		}
	}))
	defer srv.Close()

	store := newFakeTokenStore(t)
	enrollment, _ := newFakeEnrollmentWatcher(t)
	convexClient := New(Config{BaseURL: srv.URL, OperatorName: testOperatorName})
	runnable := NewRunnable(RunnableConfig{Client: convexClient, Store: store, Enrollment: enrollment, HeartbeatInterval: time.Hour})

	if err := runnable.loadOrRegister(context.Background()); err != nil {
		t.Fatalf("loadOrRegister: %v", err)
	}
	if got := registerCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 register call, got %d", got)
	}
	if runnable.CurrentDeployToken() != testDeployTokenValue {
		t.Fatalf("expected deploy token dp-1, got %q", runnable.CurrentDeployToken())
	}

	persisted, ok, err := store.Load(context.Background())
	if err != nil || !ok {
		t.Fatalf("expected token to be persisted, ok=%v err=%v", ok, err)
	}
	if persisted.HeartbeatToken != testHeartbeatToken {
		t.Fatalf("expected persisted heartbeat token hb-1, got %q", persisted.HeartbeatToken)
	}
}

// TestHeartbeatOnceReregistersOnRejection exercises the "convex rejected our
// token" path: the first heartbeat call returns 401, which must trigger a
// fresh Register call and rotate the in-memory + persisted tokens.
func TestHeartbeatOnceReregistersOnRejection(t *testing.T) {
	var heartbeatCalls, registerCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/operators/heartbeat":
			heartbeatCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
		case pathOperatorsRegister:
			registerCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(registerResponse{HeartbeatToken: "hb-2", DeployToken: "dp-2"})
		}
	}))
	defer srv.Close()

	store := newFakeTokenStore(t)
	enrollment, _ := newFakeEnrollmentWatcher(t)
	convexClient := New(Config{BaseURL: srv.URL, OperatorName: testOperatorName})
	runnable := NewRunnable(RunnableConfig{Client: convexClient, Store: store, Enrollment: enrollment, HeartbeatInterval: time.Hour})
	runnable.setTokens(tokenstore.Tokens{HeartbeatToken: "hb-stale", DeployToken: "dp-stale"})

	runnable.heartbeatOnce(context.Background())

	if heartbeatCalls.Load() != 1 {
		t.Fatalf("expected 1 heartbeat attempt")
	}
	if registerCalls.Load() != 1 {
		t.Fatalf("expected rejection to trigger exactly 1 re-registration")
	}
	if runnable.CurrentDeployToken() != "dp-2" {
		t.Fatalf("expected rotated deploy token dp-2, got %q", runnable.CurrentDeployToken())
	}
}

// TestCheckEnrollmentSecretReregistersOnChange exercises the rotation path:
// a human re-running the kubectl create secret step to change
// ENROLLMENT_SECRET must trigger a fresh Register call using the new value,
// without needing the operator pod restarted.
func TestCheckEnrollmentSecretReregistersOnChange(t *testing.T) {
	var registerCalls atomic.Int32
	var lastSecret atomic.Value

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathOperatorsRegister {
			t.Fatalf("unexpected call to %s", r.URL.Path)
		}
		registerCalls.Add(1)
		var req registerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding register request: %v", err)
		}
		lastSecret.Store(req.EnrollmentSecret)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(registerResponse{HeartbeatToken: "hb-rotated", DeployToken: "dp-rotated"})
	}))
	defer srv.Close()

	store := newFakeTokenStore(t)
	enrollment, fakeClient := newFakeEnrollmentWatcher(t)
	convexClient := New(Config{BaseURL: srv.URL, OperatorName: testOperatorName, EnrollmentSecret: "old-secret"})
	runnable := NewRunnable(RunnableConfig{Client: convexClient, Store: store, Enrollment: enrollment, HeartbeatInterval: time.Hour})
	runnable.setTokens(tokenstore.Tokens{HeartbeatToken: "hb-1", DeployToken: "dp-1"})

	putEnrollmentSecret(t, fakeClient, "new-secret")

	runnable.checkEnrollmentSecret(context.Background())

	if registerCalls.Load() != 1 {
		t.Fatalf("expected exactly 1 re-registration, got %d", registerCalls.Load())
	}
	if got := lastSecret.Load(); got != "new-secret" {
		t.Fatalf("expected register call to use rotated secret, got %v", got)
	}
	if convexClient.EnrollmentSecret() != "new-secret" {
		t.Fatalf("expected client's enrollment secret to be updated, got %q", convexClient.EnrollmentSecret())
	}
	if runnable.CurrentDeployToken() != "dp-rotated" {
		t.Fatalf("expected rotated deploy token, got %q", runnable.CurrentDeployToken())
	}
}

// TestCheckEnrollmentSecretNoopWhenUnchanged guards against re-registering
// on every heartbeat tick when nothing has actually rotated.
func TestCheckEnrollmentSecretNoopWhenUnchanged(t *testing.T) {
	var registerCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registerCalls.Add(1)
	}))
	defer srv.Close()

	store := newFakeTokenStore(t)
	enrollment, fakeClient := newFakeEnrollmentWatcher(t)
	convexClient := New(Config{BaseURL: srv.URL, OperatorName: testOperatorName, EnrollmentSecret: "same-secret"})
	runnable := NewRunnable(RunnableConfig{Client: convexClient, Store: store, Enrollment: enrollment, HeartbeatInterval: time.Hour})

	putEnrollmentSecret(t, fakeClient, "same-secret")

	runnable.checkEnrollmentSecret(context.Background())

	if registerCalls.Load() != 0 {
		t.Fatalf("expected no re-registration when enrollment secret is unchanged")
	}
}

// TestCheckEnrollmentSecretNoopWhenSecretMissing guards the common startup
// window before ai-cloud-operator-env exists at all (or momentarily during a
// Get error) — it must not treat "can't read the Secret" as "rotate to
// empty".
func TestCheckEnrollmentSecretNoopWhenSecretMissing(t *testing.T) {
	var registerCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registerCalls.Add(1)
	}))
	defer srv.Close()

	store := newFakeTokenStore(t)
	enrollment, _ := newFakeEnrollmentWatcher(t)
	convexClient := New(Config{BaseURL: srv.URL, OperatorName: testOperatorName, EnrollmentSecret: "some-secret"})
	runnable := NewRunnable(RunnableConfig{Client: convexClient, Store: store, Enrollment: enrollment, HeartbeatInterval: time.Hour})

	runnable.checkEnrollmentSecret(context.Background())

	if registerCalls.Load() != 0 {
		t.Fatalf("expected no re-registration when the enrollment secret can't be read")
	}
}

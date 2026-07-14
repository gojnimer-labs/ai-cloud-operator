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

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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
	client := New(Config{BaseURL: srv.URL, OperatorName: testOperatorName})
	runnable := NewRunnable(client, store, time.Hour)

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
	client := New(Config{BaseURL: srv.URL, OperatorName: testOperatorName})
	runnable := NewRunnable(client, store, time.Hour)
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

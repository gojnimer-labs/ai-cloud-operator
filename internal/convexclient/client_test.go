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
	"testing"
)

func TestRegisterReturnsIssuedTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/operators/register" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req registerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		if req.EnrollmentSecret != "shh" {
			t.Fatalf("expected enrollment secret to be forwarded, got %q", req.EnrollmentSecret)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(registerResponse{HeartbeatToken: "hb-1", DeployToken: "dp-1"})
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, EnrollmentSecret: "shh", OperatorName: "op-1", ExternalURL: "http://op-1"})

	tokens, err := c.Register(context.Background())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if tokens.HeartbeatToken != "hb-1" || tokens.DeployToken != "dp-1" {
		t.Fatalf("unexpected tokens: %+v", tokens)
	}
}

func TestHeartbeatSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer hb-1" {
			t.Fatalf("unexpected auth header: %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, OperatorName: "op-1"})
	if err := c.Heartbeat(context.Background(), "hb-1"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
}

func TestHeartbeatUnauthorizedMapsToSentinel(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusGone} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))

		c := New(Config{BaseURL: srv.URL, OperatorName: "op-1"})
		err := c.Heartbeat(context.Background(), "stale-token")
		srv.Close()

		if err != ErrUnauthorized {
			t.Fatalf("status %d: expected ErrUnauthorized, got %v", status, err)
		}
	}
}

func TestUpsertWorkloadSendsExpectedPayload(t *testing.T) {
	var got upsertWorkloadRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/operators/workloads/upsert" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer hb-1" {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	err := c.UpsertWorkload(context.Background(), "hb-1", WorkloadInfo{
		Name:         "demo",
		Namespace:    "default",
		Subdomain:    "demo-sub",
		TemplateName: "nginx",
		UserID:       "user-1",
	})
	if err != nil {
		t.Fatalf("upsert workload: %v", err)
	}
	if got.Name != "demo" || got.Namespace != "default" || got.TemplateID != "nginx" || got.UserID != "user-1" || got.Subdomain != "demo-sub" {
		t.Fatalf("unexpected payload: %+v", got)
	}
}

func TestUpsertWorkloadNonOKIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	if err := c.UpsertWorkload(context.Background(), "hb-1", WorkloadInfo{Name: "demo", Namespace: "default"}); err == nil {
		t.Fatalf("expected an error on non-200 response")
	}
}

func TestVerifyGatewayTokenSendsExpectedPayload(t *testing.T) {
	var got verifyGatewayTokenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/operators/gateway/verify" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer hb-1" {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(verifyGatewayTokenResponse{UserID: "user-1"})
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	userID, err := c.VerifyGatewayToken(context.Background(), "hb-1", "one-time-token", "default", "demo")
	if err != nil {
		t.Fatalf("verify gateway token: %v", err)
	}
	if userID != "user-1" {
		t.Fatalf("expected userId user-1, got %q", userID)
	}
	if got.Token != "one-time-token" || got.Namespace != "default" || got.Name != "demo" {
		t.Fatalf("unexpected payload: %+v", got)
	}
}

func TestVerifyGatewayTokenNonOKIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	if _, err := c.VerifyGatewayToken(context.Background(), "hb-1", "bad-token", "default", "demo"); err == nil {
		t.Fatalf("expected an error on non-200 response")
	}
}

func TestRemoveWorkloadSendsExpectedPayload(t *testing.T) {
	var got removeWorkloadRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/operators/workloads/remove" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	if err := c.RemoveWorkload(context.Background(), "hb-1", "demo", "default"); err != nil {
		t.Fatalf("remove workload: %v", err)
	}
	if got.Name != "demo" || got.Namespace != "default" {
		t.Fatalf("unexpected payload: %+v", got)
	}
}

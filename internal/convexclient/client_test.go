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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gojnimer-labs/ai-cloud-operator/internal/capacity"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/catalog"
)

// Shared fixture values across this package's tests (client_test.go and
// runnable_test.go) — extracted so the same token/name/path is never typed
// twice with a chance to drift out of sync.
const (
	pathOperatorsRegister       = "/operators/register"
	pathOperatorsClaim          = "/operators/workloads/claim"
	pathOperatorsClaimOperation = "/operators/workloads/claim-operation"
	pathOperatorsLifecycle      = "/operators/workloads/lifecycle"
	testHeartbeatToken          = "hb-1"
	testDeployTokenValue        = "dp-1"
	testOperatorName            = "op-1"
	testWorkloadName            = "demo"
	testNamespace               = "default"
	testUserID                  = "user-1"
	testTemplateID              = "nginx"
	testSubdomain               = "demo-sub"
	testClaimWorkloadID         = "wl-1"

	testBearerHeartbeatToken = "Bearer " + testHeartbeatToken
)

func TestRegisterReturnsIssuedTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathOperatorsRegister {
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
		_ = json.NewEncoder(w).Encode(registerResponse{HeartbeatToken: testHeartbeatToken, DeployToken: testDeployTokenValue})
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, EnrollmentSecret: "shh", OperatorName: testOperatorName, ExternalURL: "http://" + testOperatorName})

	tokens, err := c.Register(context.Background())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if tokens.HeartbeatToken != testHeartbeatToken || tokens.DeployToken != testDeployTokenValue {
		t.Fatalf("unexpected tokens: %+v", tokens)
	}
}

// TestRegisterSendsCurrentCatalog is the concrete proof that Register's
// request body carries the operator's full template catalog — the wire
// shape Convex's operators.catalog persistence and claim-time template-
// version gate depend on (see internal/catalog.List's json tags).
func TestRegisterSendsCurrentCatalog(t *testing.T) {
	var req registerRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(registerResponse{HeartbeatToken: testHeartbeatToken, DeployToken: testDeployTokenValue})
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, OperatorName: testOperatorName})
	if _, err := c.Register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}

	want, err := json.Marshal(catalog.List())
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	got, err := json.Marshal(req.Catalog)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("expected register request's catalog to match catalog.List(), got %s want %s", got, want)
	}
}

func TestHeartbeatSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != testBearerHeartbeatToken {
			t.Fatalf("unexpected auth header: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"claimable": [], "pendingOperations": []}`))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, OperatorName: testOperatorName})
	claimable, pendingOps, err := c.Heartbeat(context.Background(), testHeartbeatToken, nil)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if len(claimable) != 0 || len(pendingOps) != 0 {
		t.Fatalf("expected empty claimable/pendingOps for an empty-list 200 body, got %+v / %+v", claimable, pendingOps)
	}
}

// TestHeartbeatDecodesClaimableAndPendingOperations is the concrete proof of
// the new two-list response shape (see convex/operators/http.ts's A8 change
// in the plan): {claimable: [{workloadId, templateId}], pendingOperations:
// [{workloadId, operation}]}.
func TestHeartbeatDecodesClaimableAndPendingOperations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"claimable": [{"workloadId": "wl-1", "templateId": "nginx"}],
			"pendingOperations": [{"workloadId": "wl-2", "operation": "destroy"}]
		}`))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, OperatorName: testOperatorName})
	claimable, pendingOps, err := c.Heartbeat(context.Background(), testHeartbeatToken, nil)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if len(claimable) != 1 || claimable[0] != (ClaimableWorkload{WorkloadID: testClaimWorkloadID, TemplateID: testTemplateID}) {
		t.Fatalf("unexpected claimable: %+v", claimable)
	}
	if len(pendingOps) != 1 || pendingOps[0] != (PendingOperation{WorkloadID: "wl-2", Operation: "destroy"}) {
		t.Fatalf("unexpected pendingOps: %+v", pendingOps)
	}
}

func TestHeartbeatSendsResourceCapacityWhenSnapshotProvided(t *testing.T) {
	var got heartbeatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"claimable": [], "pendingOperations": []}`))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, OperatorName: testOperatorName})
	snap := &capacity.Snapshot{AllocatableMilliCPU: 4000, AllocatableMemoryBytes: 8 * 1024 * 1024 * 1024, UsedMilliCPU: 1000, UsedMemoryBytes: 1024 * 1024 * 1024}
	if _, _, err := c.Heartbeat(context.Background(), testHeartbeatToken, snap); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if got.ResourceCapacity == nil {
		t.Fatalf("expected resourceCapacity to be sent when a snapshot is provided")
	}
	if got.ResourceCapacity.AllocatableMilliCPU != snap.AllocatableMilliCPU || got.ResourceCapacity.UsedMemoryBytes != snap.UsedMemoryBytes {
		t.Fatalf("unexpected resourceCapacity payload: %+v", got.ResourceCapacity)
	}
}

func TestHeartbeatOmitsResourceCapacityWhenSnapshotNil(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"claimable": [], "pendingOperations": []}`))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, OperatorName: testOperatorName})
	if _, _, err := c.Heartbeat(context.Background(), testHeartbeatToken, nil); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if strings.Contains(string(gotBody), "resourceCapacity") {
		t.Fatalf("expected resourceCapacity to be omitted when snapshot is nil, got body: %s", gotBody)
	}
}

func TestHeartbeatUnauthorizedMapsToSentinel(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusGone} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))

		c := New(Config{BaseURL: srv.URL, OperatorName: testOperatorName})
		_, _, err := c.Heartbeat(context.Background(), "stale-token", nil)
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
		if got := r.Header.Get("Authorization"); got != testBearerHeartbeatToken {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	err := c.UpsertWorkload(context.Background(), testHeartbeatToken, WorkloadInfo{
		Name:         testWorkloadName,
		Namespace:    testNamespace,
		Subdomain:    testSubdomain,
		TemplateName: testTemplateID,
		UserID:       testUserID,
	})
	if err != nil {
		t.Fatalf("upsert workload: %v", err)
	}
	if got.Name != testWorkloadName || got.Namespace != testNamespace || got.TemplateID != testTemplateID || got.UserID != testUserID || got.Subdomain != testSubdomain {
		t.Fatalf("unexpected payload: %+v", got)
	}
	if got.WorkloadID != "" {
		t.Fatalf("expected no workloadId when WorkloadInfo.WorkloadID is unset, got %q", got.WorkloadID)
	}
}

// TestUpsertWorkloadCarriesWorkloadID is the concrete proof that a
// claim-flow-created CR's apps.aicloud.dev/workload-id label value makes it
// all the way into the upsert request body — the direct-by-_id correlation
// Convex's record mutation needs for its very first sync (see A4 in the
// plan).
func TestUpsertWorkloadCarriesWorkloadID(t *testing.T) {
	var got upsertWorkloadRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	err := c.UpsertWorkload(context.Background(), testHeartbeatToken, WorkloadInfo{
		Name:         testWorkloadName,
		Namespace:    testNamespace,
		TemplateName: testTemplateID,
		UserID:       testUserID,
		WorkloadID:   "convex-row-id-1",
	})
	if err != nil {
		t.Fatalf("upsert workload: %v", err)
	}
	if got.WorkloadID != "convex-row-id-1" {
		t.Fatalf("expected workloadId to be carried through, got %q", got.WorkloadID)
	}
}

func TestUpsertWorkloadNonOKIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	if err := c.UpsertWorkload(context.Background(), testHeartbeatToken, WorkloadInfo{Name: testWorkloadName, Namespace: testNamespace}); err == nil {
		t.Fatalf("expected an error on non-200 response")
	}
}

func TestVerifyGatewayTokenSendsExpectedPayload(t *testing.T) {
	var got verifyGatewayTokenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/operators/gateway/verify" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != testBearerHeartbeatToken {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(verifyGatewayTokenResponse{UserID: testUserID})
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	userID, err := c.VerifyGatewayToken(context.Background(), testHeartbeatToken, "one-time-token", testNamespace, testWorkloadName)
	if err != nil {
		t.Fatalf("verify gateway token: %v", err)
	}
	if userID != testUserID {
		t.Fatalf("expected userId user-1, got %q", userID)
	}
	if got.Token != "one-time-token" || got.Namespace != testNamespace || got.Name != testWorkloadName {
		t.Fatalf("unexpected payload: %+v", got)
	}
}

func TestVerifyGatewayTokenNonOKIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	if _, err := c.VerifyGatewayToken(context.Background(), testHeartbeatToken, "bad-token", testNamespace, testWorkloadName); err == nil {
		t.Fatalf("expected an error on non-200 response")
	}
}

func TestClaimWorkloadDecodesSuccessResponse(t *testing.T) {
	var got claimWorkloadRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathOperatorsClaim {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != testBearerHeartbeatToken {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"workloadId": "wl-1",
			"config": {"replicas": 2},
			"subdomain": "demo-sub",
			"templateId": "nginx",
			"templateVersion": "1.0.0",
			"userId": "user-1"
		}`))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	claimed, err := c.ClaimWorkload(context.Background(), testHeartbeatToken, testClaimWorkloadID)
	if err != nil {
		t.Fatalf("claim workload: %v", err)
	}
	if claimed == nil {
		t.Fatalf("expected a non-nil claimed workload")
	}
	if claimed.WorkloadID != testClaimWorkloadID || claimed.Subdomain != testSubdomain || claimed.TemplateID != testTemplateID ||
		claimed.TemplateVersion != "1.0.0" || claimed.UserID != "user-1" {
		t.Fatalf("unexpected claimed workload: %+v", claimed)
	}
	if got, ok := claimed.Config["replicas"]; !ok || got != float64(2) {
		t.Fatalf("expected config.replicas == 2, got %+v", claimed.Config)
	}
	if got.WorkloadID != testClaimWorkloadID {
		t.Fatalf("expected request to carry workloadId, got %+v", got)
	}
}

// TestClaimWorkloadLostRaceReturnsNilNil is the concrete proof that a
// 404/409 from Convex (another operator already claimed it, or it's no
// longer claimable) is reported as (nil, nil) — not an error — so the
// claim-consumption loop just skips it.
func TestClaimWorkloadLostRaceReturnsNilNil(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusConflict} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))

		c := New(Config{BaseURL: srv.URL})
		claimed, err := c.ClaimWorkload(context.Background(), testHeartbeatToken, testClaimWorkloadID)
		srv.Close()

		if err != nil {
			t.Fatalf("status %d: expected no error, got %v", status, err)
		}
		if claimed != nil {
			t.Fatalf("status %d: expected nil claimed workload, got %+v", status, claimed)
		}
	}
}

func TestClaimOperationDecodesSuccessResponse(t *testing.T) {
	var got claimOperationRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathOperatorsClaimOperation {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"operation": "redeploy",
			"name": "nginx-abc123",
			"namespace": "default",
			"config": {"replicas": 3},
			"templateId": "nginx",
			"templateVersion": "1.0.0"
		}`))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	op, err := c.ClaimOperation(context.Background(), testHeartbeatToken, testClaimWorkloadID)
	if err != nil {
		t.Fatalf("claim operation: %v", err)
	}
	if op == nil {
		t.Fatalf("expected a non-nil claimed operation")
	}
	if op.Operation != "redeploy" || op.Name != "nginx-abc123" || op.Namespace != "default" ||
		op.TemplateID != testTemplateID || op.TemplateVersion != "1.0.0" {
		t.Fatalf("unexpected claimed operation: %+v", op)
	}
	if got.WorkloadID != testClaimWorkloadID {
		t.Fatalf("expected request to carry workloadId, got %+v", got)
	}
}

func TestClaimOperationLostRaceReturnsNilNil(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusConflict} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))

		c := New(Config{BaseURL: srv.URL})
		op, err := c.ClaimOperation(context.Background(), testHeartbeatToken, testClaimWorkloadID)
		srv.Close()

		if err != nil {
			t.Fatalf("status %d: expected no error, got %v", status, err)
		}
		if op != nil {
			t.Fatalf("status %d: expected nil claimed operation, got %+v", status, op)
		}
	}
}

func TestReportLifecycleSendsExpectedPayload(t *testing.T) {
	var got reportLifecycleRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathOperatorsLifecycle {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != testBearerHeartbeatToken {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	if err := c.ReportLifecycle(context.Background(), testHeartbeatToken, testWorkloadName, testClaimWorkloadID, "failed", "boom", true); err != nil {
		t.Fatalf("report lifecycle: %v", err)
	}
	if got.Name != testWorkloadName || got.WorkloadID != testClaimWorkloadID || got.Phase != "failed" || got.Reason != "boom" || !got.Retryable {
		t.Fatalf("unexpected payload: %+v", got)
	}
}

func TestReportLifecycleOmitsRetryableWhenFalse(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	if err := c.ReportLifecycle(context.Background(), testHeartbeatToken, testWorkloadName, "", "active", "", false); err != nil {
		t.Fatalf("report lifecycle: %v", err)
	}
	if strings.Contains(string(gotBody), "retryable") {
		t.Fatalf("expected retryable to be omitted when false, got body: %s", gotBody)
	}
}

func TestReportLifecycleNonOKIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	if err := c.ReportLifecycle(context.Background(), testHeartbeatToken, testWorkloadName, "", "active", "", false); err == nil {
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
	if err := c.RemoveWorkload(context.Background(), testHeartbeatToken, testWorkloadName, testNamespace); err != nil {
		t.Fatalf("remove workload: %v", err)
	}
	if got.Name != testWorkloadName || got.Namespace != testNamespace {
		t.Fatalf("unexpected payload: %+v", got)
	}
}

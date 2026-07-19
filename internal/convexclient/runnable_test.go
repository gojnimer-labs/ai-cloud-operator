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
	"errors"
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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	appsv1alpha1 "github.com/gojnimer-labs/ai-cloud-operator/api/v1alpha1"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/capacity"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/catalog"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/provisioning"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/tokenstore"
)

// newFakeWorkloadClient returns a fake controller-runtime client that knows
// about the Workload CRD, for processClaimable/processPendingOperations
// tests that exercise a real WorkloadCreator/WorkloadDestroyer rather than
// mocking them.
func newFakeWorkloadClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding core scheme: %v", err)
	}
	if err := appsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding workload scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1alpha1.Workload{}).Build()
}

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
	if persisted.CatalogHash != catalog.Hash() {
		t.Fatalf("expected persisted catalog hash to match the current catalog, got %q", persisted.CatalogHash)
	}
}

// TestLoadOrRegisterReusesTokenWhenCatalogHashMatches confirms the
// unchanged-catalog case still short-circuits into pure reuse — no register
// call — when the persisted CatalogHash matches the running binary's
// current catalog.
func TestLoadOrRegisterReusesTokenWhenCatalogHashMatches(t *testing.T) {
	var registerCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathOperatorsHeartbeat:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"claimable": [], "pendingOperations": []}`))
		case pathOperatorsRegister:
			registerCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(registerResponse{HeartbeatToken: "hb-new", DeployToken: testDeployTokenRotated})
		}
	}))
	defer srv.Close()

	store := newFakeTokenStore(t)
	if err := store.Save(context.Background(), tokenstore.Tokens{HeartbeatToken: testHeartbeatToken, DeployToken: testDeployTokenValue, CatalogHash: catalog.Hash()}); err != nil {
		t.Fatalf("seeding store: %v", err)
	}
	enrollment, _ := newFakeEnrollmentWatcher(t)
	convexClient := New(Config{BaseURL: srv.URL, OperatorName: testOperatorName})
	runnable := NewRunnable(RunnableConfig{Client: convexClient, Store: store, Enrollment: enrollment, HeartbeatInterval: time.Hour})

	if err := runnable.loadOrRegister(context.Background()); err != nil {
		t.Fatalf("loadOrRegister: %v", err)
	}
	if registerCalls.Load() != 0 {
		t.Fatalf("expected no re-registration when catalog hash is unchanged, got %d calls", registerCalls.Load())
	}
	if runnable.CurrentDeployToken() != testDeployTokenValue {
		t.Fatalf("expected the persisted deploy token to be reused, got %q", runnable.CurrentDeployToken())
	}
}

// TestLoadOrRegisterReregistersWhenCatalogHashDiffers is the core new
// behavior: a persisted token that's still perfectly valid (heartbeat
// succeeds) must still trigger a fresh Register call when this operator's
// own compiled-in catalog no longer matches what was last reported — e.g. a
// developer bumped a Template's Version and redeployed the binary, but the
// token Secret survived the restart.
func TestLoadOrRegisterReregistersWhenCatalogHashDiffers(t *testing.T) {
	var registerCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathOperatorsHeartbeat:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"claimable": [], "pendingOperations": []}`))
		case pathOperatorsRegister:
			registerCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(registerResponse{HeartbeatToken: "hb-new", DeployToken: testDeployTokenRotated})
		}
	}))
	defer srv.Close()

	store := newFakeTokenStore(t)
	if err := store.Save(context.Background(), tokenstore.Tokens{HeartbeatToken: testHeartbeatToken, DeployToken: testDeployTokenValue, CatalogHash: "stale-hash"}); err != nil {
		t.Fatalf("seeding store: %v", err)
	}
	enrollment, _ := newFakeEnrollmentWatcher(t)
	convexClient := New(Config{BaseURL: srv.URL, OperatorName: testOperatorName})
	runnable := NewRunnable(RunnableConfig{Client: convexClient, Store: store, Enrollment: enrollment, HeartbeatInterval: time.Hour})

	if err := runnable.loadOrRegister(context.Background()); err != nil {
		t.Fatalf("loadOrRegister: %v", err)
	}
	if registerCalls.Load() != 1 {
		t.Fatalf("expected exactly 1 re-registration when catalog hash differs, got %d", registerCalls.Load())
	}
	if runnable.CurrentDeployToken() != testDeployTokenRotated {
		t.Fatalf("expected rotated deploy token %q, got %q", testDeployTokenRotated, runnable.CurrentDeployToken())
	}

	persisted, ok, err := store.Load(context.Background())
	if err != nil || !ok {
		t.Fatalf("expected freshly-registered token to be persisted, ok=%v err=%v", ok, err)
	}
	if persisted.CatalogHash != catalog.Hash() {
		t.Fatalf("expected the freshly persisted CatalogHash to match the current catalog, got %q", persisted.CatalogHash)
	}
}

// TestHeartbeatOnceReregistersOnRejection exercises the "convex rejected our
// token" path: the first heartbeat call returns 401, which must trigger a
// fresh Register call and rotate the in-memory + persisted tokens.
func TestHeartbeatOnceReregistersOnRejection(t *testing.T) {
	var heartbeatCalls, registerCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathOperatorsHeartbeat:
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

// --- processClaimable / processPendingOperations: retryable reporting ------

// TestProcessClaimableReportsRetryableOnCreateFailure forces Create to fail
// via an invalid config (out-of-range workerConnections) rather than a fake-
// client trick — a deterministic, real failure through the same
// catalog.ResolveParams validation path Create actually uses.
func TestProcessClaimableReportsRetryableOnCreateFailure(t *testing.T) {
	var lifecycle reportLifecycleRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathOperatorsClaim:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(claimOperationClaimStub())
		case pathOperatorsLifecycle:
			if err := json.NewDecoder(r.Body).Decode(&lifecycle); err != nil {
				t.Fatalf("decoding lifecycle request: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected call to %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	convexClient := New(Config{BaseURL: srv.URL})
	runnable := NewRunnable(RunnableConfig{
		Client:  convexClient,
		Creator: provisioning.NewWorkloadCreator(newFakeWorkloadClient(t), testNamespace),
	})

	runnable.processClaimable(context.Background(), testHeartbeatToken, []ClaimableWorkload{{WorkloadID: testClaimWorkloadID, TemplateID: testTemplateID}}, nil)

	if lifecycle.Phase != lifecyclePhaseFailed || !lifecycle.Retryable {
		t.Fatalf("expected a retryable failed report, got %+v", lifecycle)
	}
	if lifecycle.WorkloadID != testClaimWorkloadID {
		t.Fatalf("expected workloadId %q, got %+v", testClaimWorkloadID, lifecycle)
	}
}

// claimOperationClaimStub returns a claim response whose config fails
// nginx's own workerConnections validation (Max: 65536), so Create fails
// deterministically for TestProcessClaimableReportsRetryableOnCreateFailure.
func claimOperationClaimStub() claimWorkloadResponse {
	return claimWorkloadResponse{
		WorkloadID:      testClaimWorkloadID,
		Config:          map[string]any{"workerConnections": float64(999_999)},
		Subdomain:       testSubdomain,
		TemplateID:      testTemplateID,
		TemplateVersion: catalog.Nginx.Version,
		UserID:          testUserID,
	}
}

// TestProcessClaimableTemplateVersionMismatchIsNotRetryable confirms the
// genuinely-terminal template-version-mismatch path is unaffected by the new
// retryable plumbing — it must stay non-retryable.
func TestProcessClaimableTemplateVersionMismatchIsNotRetryable(t *testing.T) {
	var lifecycle reportLifecycleRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathOperatorsClaim:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(claimWorkloadResponse{
				WorkloadID:      testClaimWorkloadID,
				TemplateID:      testTemplateID,
				TemplateVersion: "stale-version",
				UserID:          testUserID,
			})
		case pathOperatorsLifecycle:
			if err := json.NewDecoder(r.Body).Decode(&lifecycle); err != nil {
				t.Fatalf("decoding lifecycle request: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected call to %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	convexClient := New(Config{BaseURL: srv.URL})
	runnable := NewRunnable(RunnableConfig{
		Client:  convexClient,
		Creator: provisioning.NewWorkloadCreator(newFakeWorkloadClient(t), testNamespace),
	})

	runnable.processClaimable(context.Background(), testHeartbeatToken, []ClaimableWorkload{{WorkloadID: testClaimWorkloadID, TemplateID: testTemplateID}}, nil)

	if lifecycle.Phase != lifecyclePhaseFailed || lifecycle.Retryable {
		t.Fatalf("expected a non-retryable failed report on template-version mismatch, got %+v", lifecycle)
	}
	if lifecycle.Reason != reasonTemplateVersionMismatch {
		t.Fatalf("expected template-version-mismatch reason, got %+v", lifecycle)
	}
}

// TestProcessPendingOperationsReportsRetryableOnRedeployFailure forces
// Redeploy to fail via a nonexistent CR name (a real, deterministic Get
// error) rather than a fake-client trick.
func TestProcessPendingOperationsReportsRetryableOnRedeployFailure(t *testing.T) {
	var lifecycle reportLifecycleRequest
	const missingName = "does-not-exist"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathOperatorsClaimOperation:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(claimOperationResponse{
				Operation:       operationRedeploy,
				Name:            missingName,
				Namespace:       testNamespace,
				TemplateID:      testTemplateID,
				TemplateVersion: catalog.Nginx.Version,
			})
		case pathOperatorsLifecycle:
			if err := json.NewDecoder(r.Body).Decode(&lifecycle); err != nil {
				t.Fatalf("decoding lifecycle request: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected call to %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	convexClient := New(Config{BaseURL: srv.URL})
	runnable := NewRunnable(RunnableConfig{
		Client:  convexClient,
		Creator: provisioning.NewWorkloadCreator(newFakeWorkloadClient(t), testNamespace),
	})

	runnable.processPendingOperations(context.Background(), testHeartbeatToken, []PendingOperation{{WorkloadID: testClaimWorkloadID, Operation: operationRedeploy}})

	if lifecycle.Phase != lifecyclePhaseFailed || !lifecycle.Retryable {
		t.Fatalf("expected a retryable failed report, got %+v", lifecycle)
	}
	if lifecycle.Name != missingName {
		t.Fatalf("expected name %q, got %+v", missingName, lifecycle)
	}
}

// TestProcessPendingOperationsReportsRetryableOnSetSuspendedFailure covers
// the shared stop/resume path via a nonexistent CR name, same shape as the
// redeploy failure test above.
func TestProcessPendingOperationsReportsRetryableOnSetSuspendedFailure(t *testing.T) {
	var lifecycle reportLifecycleRequest
	const missingName = "does-not-exist"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathOperatorsClaimOperation:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(claimOperationResponse{
				Operation: operationStop,
				Name:      missingName,
				Namespace: testNamespace,
			})
		case pathOperatorsLifecycle:
			if err := json.NewDecoder(r.Body).Decode(&lifecycle); err != nil {
				t.Fatalf("decoding lifecycle request: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected call to %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	convexClient := New(Config{BaseURL: srv.URL})
	runnable := NewRunnable(RunnableConfig{
		Client:  convexClient,
		Creator: provisioning.NewWorkloadCreator(newFakeWorkloadClient(t), testNamespace),
	})

	runnable.processPendingOperations(context.Background(), testHeartbeatToken, []PendingOperation{{WorkloadID: testClaimWorkloadID, Operation: operationStop}})

	if lifecycle.Phase != lifecyclePhaseFailed || !lifecycle.Retryable {
		t.Fatalf("expected a retryable failed report, got %+v", lifecycle)
	}
}

// TestProcessPendingOperationsReportsRetryableOnDestroyFailure closes what
// used to be a silent gap: a synchronous Destroy error previously produced
// NO lifecycle report at all (see this file's package doc / runnable.go's
// processPendingOperations comment), relying entirely on Convex re-surfacing
// the same pending destroy on a future heartbeat. Now it reports, retryable.
// NotFound is treated as success by Destroy (idempotent), so a real failure
// needs an interceptor forcing a different error out of Delete.
func TestProcessPendingOperationsReportsRetryableOnDestroyFailure(t *testing.T) {
	var lifecycle reportLifecycleRequest
	var lifecycleCalled bool
	const targetName = "some-workload"

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding core scheme: %v", err)
	}
	if err := appsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding workload scheme: %v", err)
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				return errors.New("boom: simulated delete failure")
			},
		}).
		Build()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathOperatorsClaimOperation:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(claimOperationResponse{
				Operation: operationDestroy,
				Name:      targetName,
				Namespace: testNamespace,
			})
		case pathOperatorsLifecycle:
			lifecycleCalled = true
			if err := json.NewDecoder(r.Body).Decode(&lifecycle); err != nil {
				t.Fatalf("decoding lifecycle request: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected call to %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	convexClient := New(Config{BaseURL: srv.URL})
	runnable := NewRunnable(RunnableConfig{
		Client:    convexClient,
		Destroyer: provisioning.NewWorkloadDestroyer(fakeClient, testNamespace),
	})

	runnable.processPendingOperations(context.Background(), testHeartbeatToken, []PendingOperation{{WorkloadID: testClaimWorkloadID, Operation: operationDestroy}})

	if !lifecycleCalled {
		t.Fatalf("expected a lifecycle report on destroy failure — this used to be a silent gap")
	}
	if lifecycle.Phase != lifecyclePhaseFailed || !lifecycle.Retryable {
		t.Fatalf("expected a retryable failed report, got %+v", lifecycle)
	}
	if lifecycle.Name != targetName {
		t.Fatalf("expected name %q, got %+v", targetName, lifecycle)
	}
}

// TestProcessClaimableSelfGateSkipsTooLargeButStillClaimsSmaller is the core
// self-gate behavior: a snapshot with just enough room for nginx (50m/64Mi)
// but not firefox (1000m/1500Mi) must skip the firefox candidate — never
// even calling ClaimWorkload for it, so it stays claimable for a
// better-fitting operator — while still claiming and creating the smaller
// nginx candidate later in the same tick. Also confirms the in-memory
// snapshot shrinks by nginx's estimate after that successful claim.
func TestProcessClaimableSelfGateSkipsTooLargeButStillClaimsSmaller(t *testing.T) {
	const bigWorkloadID = "wl-big"
	const smallWorkloadID = "wl-small"

	var claimedIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathOperatorsClaim:
			var req claimWorkloadRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decoding claim request: %v", err)
			}
			claimedIDs = append(claimedIDs, req.WorkloadID)
			if req.WorkloadID != smallWorkloadID {
				t.Fatalf("expected ClaimWorkload to only ever be called for %q (too-large candidate should be skipped pre-claim), got %q", smallWorkloadID, req.WorkloadID)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(claimWorkloadResponse{
				WorkloadID:      smallWorkloadID,
				TemplateID:      testTemplateID,
				TemplateVersion: catalog.Nginx.Version,
				UserID:          testUserID,
			})
		case pathOperatorsLifecycle:
			var lifecycle reportLifecycleRequest
			if err := json.NewDecoder(r.Body).Decode(&lifecycle); err != nil {
				t.Fatalf("decoding lifecycle request: %v", err)
			}
			t.Fatalf("expected no lifecycle report — the small candidate's Create should succeed, got %+v", lifecycle)
		default:
			t.Fatalf("unexpected call to %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	convexClient := New(Config{BaseURL: srv.URL})
	runnable := NewRunnable(RunnableConfig{
		Client:  convexClient,
		Creator: provisioning.NewWorkloadCreator(newFakeWorkloadClient(t), testNamespace),
	})

	firefoxCPU, firefoxMem := catalog.Firefox.EstimatedResources()
	nginxCPU, nginxMem := catalog.Nginx.EstimatedResources()
	// Room for nginx alone, deliberately less than firefox needs.
	snap := &capacity.Snapshot{AllocatableMilliCPU: nginxCPU, AllocatableMemoryBytes: nginxMem}
	if snap.Fits(firefoxCPU, firefoxMem) {
		t.Fatalf("test setup invalid: snapshot should not have room for firefox")
	}

	runnable.processClaimable(context.Background(), testHeartbeatToken, []ClaimableWorkload{
		{WorkloadID: bigWorkloadID, TemplateID: "firefox"},
		{WorkloadID: smallWorkloadID, TemplateID: testTemplateID},
	}, snap)

	if len(claimedIDs) != 1 || claimedIDs[0] != smallWorkloadID {
		t.Fatalf("expected exactly one ClaimWorkload call, for %q, got %v", smallWorkloadID, claimedIDs)
	}
	if snap.UsedMilliCPU != nginxCPU || snap.UsedMemoryBytes != nginxMem {
		t.Fatalf("expected snapshot to shrink by nginx's estimate after the successful claim, got UsedMilliCPU=%d UsedMemoryBytes=%d", snap.UsedMilliCPU, snap.UsedMemoryBytes)
	}
}

// TestProcessClaimableNilSnapshotDisablesSelfGate confirms a nil snapshot
// (no Tracker configured, or this tick's Snapshot call errored) falls back
// to today's behavior — every candidate is attempted, regardless of size.
func TestProcessClaimableNilSnapshotDisablesSelfGate(t *testing.T) {
	var claimedIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathOperatorsClaim:
			var req claimWorkloadRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decoding claim request: %v", err)
			}
			claimedIDs = append(claimedIDs, req.WorkloadID)
			w.WriteHeader(http.StatusConflict) // treated as "lost the race", no further processing needed
		default:
			t.Fatalf("unexpected call to %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	convexClient := New(Config{BaseURL: srv.URL})
	runnable := NewRunnable(RunnableConfig{
		Client:  convexClient,
		Creator: provisioning.NewWorkloadCreator(newFakeWorkloadClient(t), testNamespace),
	})

	runnable.processClaimable(context.Background(), testHeartbeatToken, []ClaimableWorkload{
		{WorkloadID: "wl-1", TemplateID: "firefox"},
		{WorkloadID: "wl-2", TemplateID: testTemplateID},
	}, nil)

	if len(claimedIDs) != 2 {
		t.Fatalf("expected both candidates attempted with no self-gate, got %v", claimedIDs)
	}
}

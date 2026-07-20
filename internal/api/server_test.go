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

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1alpha1 "github.com/gojnimer-labs/ai-cloud-operator/api/v1alpha1"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/catalog"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/gateway"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/provisioning"
)

const (
	testDeployToken   = "deploy-token-1"
	testGatewaySecret = "gateway-signing-secret-1"
	testServiceName   = "gw-demo"
	testServiceNS     = "default"
	testServicePort   = int32(8080)
	// testOneTimeToken is the only one-time token value these tests ever
	// need — each test issues it for exactly one (namespace, name) and
	// requests "?token="+testOneTimeToken, so there's never a reason for it
	// to vary.
	testOneTimeToken = "tok-1"

	testWorkloadName        = "demo"
	testImage               = "nginx:latest"
	testNginxTemplateID     = "nginx"
	testFirefoxTemplateID   = "firefox"
	testFirefoxWorkloadName = "my-firefox"
	testUserID              = "user-1"
)

// fakeGatewayVerifier stands in for a real Convex round trip: issue records
// testOneTimeToken as valid for exactly one (namespace, name), and
// VerifyGatewayToken consumes it — a second attempt to use the same token
// fails, mirroring Convex's real single-use enforcement.
type fakeGatewayVerifier struct {
	tokens map[string]struct{ namespace, name, userID string }
}

func newFakeGatewayVerifier() *fakeGatewayVerifier {
	return &fakeGatewayVerifier{tokens: map[string]struct{ namespace, name, userID string }{}}
}

func (f *fakeGatewayVerifier) issue(name string) {
	f.tokens[testOneTimeToken] = struct{ namespace, name, userID string }{testServiceNS, name, testUserID}
}

func (f *fakeGatewayVerifier) VerifyGatewayToken(_ context.Context, token, namespace, name string) (string, error) {
	claim, ok := f.tokens[token]
	if !ok || claim.namespace != namespace || claim.name != name {
		return "", errors.New("invalid or expired token")
	}
	delete(f.tokens, token)
	return claim.userID, nil
}

// podExecCall records one invocation of fakePodExecutor.Exec, for tests that
// need to assert what command ran and against which pod/container.
type podExecCall struct {
	namespace, podName, container string
	command                       []string
}

// fakePodExecutor stands in for a real client-go SPDY exec: every call
// returns the same canned (stdout, stderr, err), and is recorded in calls.
type fakePodExecutor struct {
	calls  []podExecCall
	stdout string
	stderr string
	err    error
}

func (f *fakePodExecutor) Exec(_ context.Context, namespace, podName, container string, command []string) (string, string, error) {
	f.calls = append(f.calls, podExecCall{namespace: namespace, podName: podName, container: container, command: command})
	return f.stdout, f.stderr, f.err
}

func newTestServer(t *testing.T) (*Server, client.Client, *fakeGatewayVerifier, *fakePodExecutor) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding core scheme: %v", err)
	}
	if err := appsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding workload scheme: %v", err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: testServiceNS},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: testServicePort}}},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1alpha1.Workload{}).
		WithObjects(svc).
		Build()

	proxy, err := gateway.NewServiceProxy(c, &rest.Config{Host: "http://127.0.0.1:0"}, testServiceNS)
	if err != nil {
		t.Fatalf("NewServiceProxy: %v", err)
	}

	verifier := newFakeGatewayVerifier()
	executor := &fakePodExecutor{}
	creator := provisioning.NewWorkloadCreator(c, testServiceNS)
	destroyer := provisioning.NewWorkloadDestroyer(c, testServiceNS)
	s := New(Config{
		Client:          c,
		ListenAddr:      ":0",
		Token:           func() string { return testDeployToken },
		GatewaySecret:   []byte(testGatewaySecret),
		GatewayVerifier: verifier,
		Proxy:           proxy,
		PodExecutor:     executor,
		Creator:         creator,
		Destroyer:       destroyer,
		Namespace:       testServiceNS,
	})
	return s, c, verifier, executor
}

func (s *Server) testHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.Handle("POST /workloads", s.requireDeployToken(http.HandlerFunc(s.handleDeploy)))
	mux.Handle("GET /workloads/{name}", s.requireDeployToken(http.HandlerFunc(s.handleGet)))
	mux.Handle("DELETE /workloads/{name}", s.requireDeployToken(http.HandlerFunc(s.handleDelete)))
	mux.Handle("GET /gw/{name}/{entrypoint}/{subpath...}", s.requireGatewayToken(s.proxy.Handler()))
	mux.Handle("POST /workloads/{name}/functions/{key}", s.requireDeployToken(http.HandlerFunc(s.handleRunFunction)))
	return mux
}

func TestHealthzIsUnauthenticated(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestWorkloadsRequiresBearerToken(t *testing.T) {
	s, _, _, _ := newTestServer(t)

	cases := []struct {
		name   string
		header string
	}{
		{"missing header", ""},
		{"wrong token", "Bearer wrong-token"},
		{"malformed header", "deploy-token-1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/workloads/demo", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()

			s.testHandler().ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", rec.Code)
			}
		})
	}
}

func TestDeployCreatesWorkloadCR(t *testing.T) {
	s, c, _, _ := newTestServer(t)

	body, _ := json.Marshal(deployRequest{
		Name:          testWorkloadName,
		Image:         testImage,
		ContainerPort: 8080,
	})
	req := httptest.NewRequest(http.MethodPost, "/workloads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDeployToken)
	rec := httptest.NewRecorder()

	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	var workload appsv1alpha1.Workload
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testServiceNS, Name: testWorkloadName}, &workload); err != nil {
		t.Fatalf("expected workload CR to be created: %v", err)
	}
	if workload.Spec.Image != testImage {
		t.Fatalf("expected image nginx:latest, got %q", workload.Spec.Image)
	}
}

func TestDeployRejectsMissingFields(t *testing.T) {
	s, _, _, _ := newTestServer(t)

	body, _ := json.Marshal(deployRequest{Name: testWorkloadName})
	req := httptest.NewRequest(http.MethodPost, "/workloads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDeployToken)
	rec := httptest.NewRecorder()

	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestGetReturns404ForUnknownWorkload(t *testing.T) {
	s, _, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/workloads/does-not-exist", nil)
	req.Header.Set("Authorization", "Bearer "+testDeployToken)
	rec := httptest.NewRecorder()

	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestDeleteRemovesWorkloadCR(t *testing.T) {
	s, c, _, _ := newTestServer(t)
	ctx := context.Background()

	workload := &appsv1alpha1.Workload{}
	workload.Name = testWorkloadName
	workload.Namespace = testServiceNS
	workload.Spec.Image = testImage
	if err := c.Create(ctx, workload); err != nil {
		t.Fatalf("seeding workload: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/workloads/demo", nil)
	req.Header.Set("Authorization", "Bearer "+testDeployToken)
	rec := httptest.NewRecorder()

	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}

	var check appsv1alpha1.Workload
	err := c.Get(ctx, client.ObjectKey{Namespace: testServiceNS, Name: testWorkloadName}, &check)
	if err == nil {
		t.Fatalf("expected workload to be deleted")
	}
}

func mintTestCookie(t *testing.T, secret, namespace, name string, exp time.Time) string {
	t.Helper()
	token, err := gateway.Sign([]byte(secret), gateway.Payload{Namespace: namespace, Name: name, Exp: exp.Unix()})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return token
}

func TestGatewayRequiresToken(t *testing.T) {
	s, _, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/gw/"+testServiceName+"/http/", nil)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no token, got %d", rec.Code)
	}
}

func TestGatewayRejectsUnknownOrWrongScopeToken(t *testing.T) {
	s, _, verifier, _ := newTestServer(t)

	// Issued for a different workload name — must not authorize this one.
	verifier.issue("some-other-workload")
	req := httptest.NewRequest(http.MethodGet, "/gw/"+testServiceName+"/http/?token="+testOneTimeToken, nil)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong-scope token, got %d", rec.Code)
	}
}

func TestGatewayRejectsWhenVerifierFails(t *testing.T) {
	s, _, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/gw/"+testServiceName+"/http/?token=bogus", nil)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a token the verifier rejects, got %d", rec.Code)
	}
}

func newTestServerWithAPIServer(t *testing.T, apiServerURL string) (*Server, *fakeGatewayVerifier) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	if err := appsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: testServiceNS},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
			{Name: "http", Port: testServicePort},
			{Name: "backoffice", Port: 9090},
		}},
	}
	// Handler() now gates on workload readiness before resolving the
	// Service — these gateway-token tests are exercising auth/proxy
	// behavior, not readiness, so the workload just needs to already be
	// Running.
	wl := &appsv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: testServiceNS},
		Status:     appsv1alpha1.WorkloadStatus{Phase: "Running"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, wl).Build()
	proxy, err := gateway.NewServiceProxy(c, &rest.Config{Host: apiServerURL}, testServiceNS)
	if err != nil {
		t.Fatalf("NewServiceProxy: %v", err)
	}
	verifier := newFakeGatewayVerifier()
	creator := provisioning.NewWorkloadCreator(c, testServiceNS)
	destroyer := provisioning.NewWorkloadDestroyer(c, testServiceNS)
	return New(Config{
		Client:          c,
		ListenAddr:      ":0",
		Token:           func() string { return testDeployToken },
		GatewaySecret:   []byte(testGatewaySecret),
		GatewayVerifier: verifier,
		Proxy:           proxy,
		PodExecutor:     &fakePodExecutor{},
		Creator:         creator,
		Destroyer:       destroyer,
		Namespace:       testServiceNS,
	}), verifier
}

// exchangeGatewayToken performs a token-bearing gateway request against the
// "http" entrypoint of testServiceName and asserts it redirects — rather
// than serving content directly on the same response — to the same path
// with ?token= stripped, carrying exactly one cookie. See
// requireGatewayToken's doc comment for why: the one-time token must never
// linger in the address bar, browser history, or a bookmark once it's been
// exchanged for a cookie. Returns that cookie for the caller's own
// follow-up request.
func exchangeGatewayToken(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	requestPath := "/gw/" + testServiceName + "/http/?token=" + testOneTimeToken
	req := httptest.NewRequest(http.MethodGet, requestPath, nil)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect after token exchange, got %d: %s", rec.Code, rec.Body.String())
	}
	wantLocation, _, _ := strings.Cut(requestPath, "?")
	if got := rec.Header().Get("Location"); got != wantLocation {
		t.Fatalf("expected redirect Location %q with token stripped, got %q", wantLocation, got)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != gatewayCookieName {
		t.Fatalf("expected a %s cookie to be set, got %+v", gatewayCookieName, cookies)
	}
	return cookies[0]
}

func TestGatewayAcceptsValidTokenAndProxies(t *testing.T) {
	// Stand in for the real kube-apiserver so we can assert the proxied
	// request actually reaches it once the gateway cookie passes.
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/v1/namespaces/" + testServiceNS + "/services/" + testServiceName + ":8080/proxy/"
		if r.URL.Path != wantPath {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte("ok from service"))
	}))
	defer apiServer.Close()

	s, verifier := newTestServerWithAPIServer(t, apiServer.URL)
	verifier.issue(testServiceName)

	cookie := exchangeGatewayToken(t, s)
	wantPath := "/gw/" + testServiceName
	if cookie.Path != wantPath {
		t.Fatalf("expected cookie Path %q, got %q", wantPath, cookie.Path)
	}

	// The browser's next request is the redirect target: same path, cookie
	// attached, no token — this is what actually has to reach the service.
	req := httptest.NewRequest(http.MethodGet, "/gw/"+testServiceName+"/http/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "ok from service" {
		t.Fatalf("unexpected proxied body: %q", rec.Body.String())
	}
}

func TestGatewayTokenIsSingleUse(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer apiServer.Close()

	s, verifier := newTestServerWithAPIServer(t, apiServer.URL)
	verifier.issue(testServiceName)

	exchangeGatewayToken(t, s)

	// Same token again, no cookie this time — the fake verifier already
	// deleted it on first use, exactly like Convex's real single-use check.
	req2 := httptest.NewRequest(http.MethodGet, "/gw/"+testServiceName+"/http/?token="+testOneTimeToken, nil)
	rec2 := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("expected second use of the same one-time token to be rejected, got %d", rec2.Code)
	}
}

func TestGatewayCookieAuthorizesSubsequentRequestsWithoutToken(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer apiServer.Close()

	s, verifier := newTestServerWithAPIServer(t, apiServer.URL)
	verifier.issue(testServiceName)

	cookie := exchangeGatewayToken(t, s)

	// A follow-up request — as a sub-resource load would be — carries the
	// cookie but no ?token=. This must succeed purely from the cookie: the
	// one-time token was already consumed above, so any accidental fallback
	// to the verifier would fail this request.
	req2 := httptest.NewRequest(http.MethodGet, "/gw/"+testServiceName+"/http/assets/app.js", nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected cookie-authenticated request to succeed, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

// TestGatewayCookieAuthorizesDifferentEntrypointsOfSameWorkload is the
// concrete "reach" proof for multi-entrypoint support: the gateway cookie is
// scoped to the workload (namespace+name), not to a specific entrypoint, so
// a cookie minted while exchanging a token against one entrypoint must also
// authorize a request against a different entrypoint of the same workload,
// with no second token exchange.
func TestGatewayCookieAuthorizesDifferentEntrypointsOfSameWorkload(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer apiServer.Close()

	s, verifier := newTestServerWithAPIServer(t, apiServer.URL)
	verifier.issue(testServiceName)

	cookie := exchangeGatewayToken(t, s)

	// Same cookie, no token, but against the "backoffice" entrypoint of the
	// same workload — must succeed purely from the workload-scoped cookie.
	req2 := httptest.NewRequest(http.MethodGet, "/gw/"+testServiceName+"/backoffice/", nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected cookie to authorize a different entrypoint of the same workload, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

// TestGatewayInvalidTokenIsNotMaskedByExistingCookie guards against a real
// reported vulnerability: requireGatewayToken used to check the session
// cookie first and only fall back to ?token= when no cookie was present —
// so once a valid cookie existed, any token value in the URL (wrong,
// stale, or an attacker's) was silently ignored instead of being
// revalidated. A token in the URL must always be checked for real,
// regardless of what cookie is already on the request — see
// requireGatewayToken's doc comment.
func TestGatewayInvalidTokenIsNotMaskedByExistingCookie(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer apiServer.Close()

	s, verifier := newTestServerWithAPIServer(t, apiServer.URL)
	verifier.issue(testServiceName)

	// Establish a legitimate, still-valid session cookie.
	cookie := exchangeGatewayToken(t, s)

	// A request carrying that valid cookie AND an invalid ?token= must
	// still be rejected: presenting a token is a deliberate
	// re-authentication attempt, not something an existing cookie should
	// be able to paper over.
	req := httptest.NewRequest(http.MethodGet, "/gw/"+testServiceName+"/http/?token=some-other-bogus-token", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when an invalid token accompanies a valid cookie, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGatewayCookieRejectedForDifferentWorkload(t *testing.T) {
	s, _, _, _ := newTestServer(t)

	// A cookie validly signed, but for a different workload name, must not
	// authorize this workload's path — defense in depth alongside the
	// cookie's own Path scoping, which a client could in principle ignore.
	cookie := mintTestCookie(t, testGatewaySecret, testServiceNS, "some-other-workload", time.Now().Add(time.Minute))
	req := httptest.NewRequest(http.MethodGet, "/gw/"+testServiceName+"/http/", nil)
	req.AddCookie(&http.Cookie{Name: gatewayCookieName, Value: cookie})
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a cookie scoped to a different workload, got %d", rec.Code)
	}
}

func TestGatewayCookieRejectedWhenExpired(t *testing.T) {
	s, _, _, _ := newTestServer(t)

	cookie := mintTestCookie(t, testGatewaySecret, testServiceNS, testServiceName, time.Now().Add(-time.Hour))
	req := httptest.NewRequest(http.MethodGet, "/gw/"+testServiceName+"/http/", nil)
	req.AddCookie(&http.Cookie{Name: gatewayCookieName, Value: cookie})
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an expired cookie with no fallback token, got %d", rec.Code)
	}
}

func TestDeployRejectsUnknownTemplate(t *testing.T) {
	s, _, _, _ := newTestServer(t)

	body, _ := json.Marshal(deployRequest{
		Name:         testWorkloadName,
		TemplateName: "does-not-exist",
	})
	req := httptest.NewRequest(http.MethodPost, "/workloads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDeployToken)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeployWithTemplateNameSkipsImageRequirement(t *testing.T) {
	s, c, _, _ := newTestServer(t)

	body, _ := json.Marshal(deployRequest{
		TemplateName: testNginxTemplateID,
		UserID:       testUserID,
	})
	req := httptest.NewRequest(http.MethodPost, "/workloads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDeployToken)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp workloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	wantPrefix := testNginxTemplateID + "-"
	if !strings.HasPrefix(resp.Name, wantPrefix) {
		t.Fatalf("expected generated name with prefix %q, got %q", wantPrefix, resp.Name)
	}

	var workload appsv1alpha1.Workload
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testServiceNS, Name: resp.Name}, &workload); err != nil {
		t.Fatalf("expected workload CR to be created at generated name %q: %v", resp.Name, err)
	}
	if workload.Spec.TemplateName != testNginxTemplateID {
		t.Fatalf("expected templateName=nginx, got %q", workload.Spec.TemplateName)
	}
}

// TestDeployIgnoresClientSuppliedNameForTemplateDeploy is the concrete proof
// that a template deploy's name always comes from the operator's own
// GenerateName mechanism — even a well-formed, distinct req.Name never
// becomes the Workload's actual name.
func TestDeployIgnoresClientSuppliedNameForTemplateDeploy(t *testing.T) {
	s, _, _, _ := newTestServer(t)

	body, _ := json.Marshal(deployRequest{
		Name:         "whatever-the-caller-sent",
		TemplateName: testNginxTemplateID,
		UserID:       testUserID,
	})
	req := httptest.NewRequest(http.MethodPost, "/workloads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDeployToken)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp workloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Name == "whatever-the-caller-sent" {
		t.Fatalf("expected the client-supplied name to be ignored, got %q", resp.Name)
	}
	wantPrefix := testNginxTemplateID + "-"
	if !strings.HasPrefix(resp.Name, wantPrefix) {
		t.Fatalf("expected generated name with prefix %q, got %q", wantPrefix, resp.Name)
	}
}

// TestDeployAllowsMultipleInstancesOfSameTemplateForSameUser is the
// end-to-end proof that repeat deploys of the same (userId, templateName)
// always create a new, distinctly-named Workload rather than the second
// call silently overwriting the first.
func TestDeployAllowsMultipleInstancesOfSameTemplateForSameUser(t *testing.T) {
	s, c, _, _ := newTestServer(t)

	deploy := func() string {
		t.Helper()
		body, _ := json.Marshal(deployRequest{
			TemplateName: testNginxTemplateID,
			UserID:       testUserID,
		})
		req := httptest.NewRequest(http.MethodPost, "/workloads", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testDeployToken)
		rec := httptest.NewRecorder()
		s.testHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp workloadResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		return resp.Name
	}

	first := deploy()
	second := deploy()

	if first == second {
		t.Fatalf("expected two deploys to produce distinct names, both resolved to %q", first)
	}

	var firstWorkload, secondWorkload appsv1alpha1.Workload
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testServiceNS, Name: first}, &firstWorkload); err != nil {
		t.Fatalf("expected first workload to exist: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testServiceNS, Name: second}, &secondWorkload); err != nil {
		t.Fatalf("expected second workload to exist: %v", err)
	}
}

func TestDeployRequiresUserIDForTemplateDeploy(t *testing.T) {
	s, _, _, _ := newTestServer(t)

	body, _ := json.Marshal(deployRequest{
		TemplateName: testNginxTemplateID,
	})
	req := httptest.NewRequest(http.MethodPost, "/workloads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDeployToken)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when userId is missing, got %d: %s", rec.Code, rec.Body.String())
	}
}

// seedRunningPod creates a pod labeled the way the reconciler labels every
// object it creates, so podexec.FindPod (used by handleRunFunction) can
// locate it for workloadName.
func seedRunningPod(t *testing.T, c client.Client, namespace, workloadName, podName string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels:    map[string]string{"app.kubernetes.io/name": workloadName},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: testFirefoxTemplateID, Image: "linuxserver/firefox:latest"}}},
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatalf("seeding pod: %v", err)
	}
	pod.Status.Phase = corev1.PodRunning
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("updating pod status: %v", err)
	}
}

func TestRunFunctionExecutesAgainstRunningPod(t *testing.T) {
	s, c, _, executor := newTestServer(t)
	executor.stdout = "irrelevant — the response no longer surfaces raw stdout"

	workload := &appsv1alpha1.Workload{}
	workload.Name = testFirefoxWorkloadName
	workload.Namespace = testServiceNS
	workload.Spec.TemplateName = testFirefoxTemplateID
	if err := c.Create(context.Background(), workload); err != nil {
		t.Fatalf("seeding workload: %v", err)
	}
	seedRunningPod(t, c, testServiceNS, testFirefoxWorkloadName, "my-firefox-abc123")

	body, _ := json.Marshal(runFunctionRequest{Params: map[string]any{"uploadUrl": "https://r2.example.com/upload"}})
	req := httptest.NewRequest(http.MethodPost, "/workloads/my-firefox/functions/backup_state", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDeployToken)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var result runFunctionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(result.AdditionalInfo) != 1 || result.AdditionalInfo[0].Name != "result" ||
		result.AdditionalInfo[0].Type != catalog.AdditionalInfoPlain ||
		result.AdditionalInfo[0].Value != "backup_state.success" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.File == nil || result.File.Type != "browser_profile_backup" {
		t.Fatalf("expected a browser_profile_backup FileResult, got %+v", result.File)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("expected exactly one exec call, got %d", len(executor.calls))
	}
	call := executor.calls[0]
	if call.podName != "my-firefox-abc123" || call.container != testFirefoxTemplateID {
		t.Fatalf("unexpected exec target: podName=%q container=%q", call.podName, call.container)
	}
}

func TestRunFunctionRejectsUnknownFunctionKey(t *testing.T) {
	s, c, _, _ := newTestServer(t)

	workload := &appsv1alpha1.Workload{}
	workload.Name = testFirefoxWorkloadName
	workload.Namespace = testServiceNS
	workload.Spec.TemplateName = testFirefoxTemplateID
	if err := c.Create(context.Background(), workload); err != nil {
		t.Fatalf("seeding workload: %v", err)
	}

	body, _ := json.Marshal(runFunctionRequest{})
	req := httptest.NewRequest(http.MethodPost, "/workloads/my-firefox/functions/does-not-exist", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDeployToken)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRunFunctionRejectsMissingRequiredParam(t *testing.T) {
	s, c, _, _ := newTestServer(t)

	workload := &appsv1alpha1.Workload{}
	workload.Name = testFirefoxWorkloadName
	workload.Namespace = testServiceNS
	workload.Spec.TemplateName = testFirefoxTemplateID
	if err := c.Create(context.Background(), workload); err != nil {
		t.Fatalf("seeding workload: %v", err)
	}

	body, _ := json.Marshal(runFunctionRequest{})
	req := httptest.NewRequest(http.MethodPost, "/workloads/my-firefox/functions/backup_state", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDeployToken)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing uploadUrl, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRunFunctionFailsWhenNoPodIsRunning(t *testing.T) {
	s, c, _, _ := newTestServer(t)

	workload := &appsv1alpha1.Workload{}
	workload.Name = testFirefoxWorkloadName
	workload.Namespace = testServiceNS
	workload.Spec.TemplateName = testFirefoxTemplateID
	if err := c.Create(context.Background(), workload); err != nil {
		t.Fatalf("seeding workload: %v", err)
	}

	body, _ := json.Marshal(runFunctionRequest{Params: map[string]any{"uploadUrl": "https://r2.example.com/upload"}})
	req := httptest.NewRequest(http.MethodPost, "/workloads/my-firefox/functions/backup_state", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDeployToken)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 when no pod is running, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRunFunctionRequiresDeployToken(t *testing.T) {
	s, _, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/workloads/my-firefox/functions/backup_state", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

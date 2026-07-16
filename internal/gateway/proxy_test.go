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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1alpha1 "github.com/gojnimer-labs/ai-cloud-operator/api/v1alpha1"
)

// readyWorkload is the Workload every pre-existing test needs seeded now
// that Handler() gates on workload readiness before ever resolving the
// Service — these tests are exercising the Service-lookup/proxy behavior,
// so they want a Workload that's simply already Running.
func readyWorkload() *appsv1alpha1.Workload {
	return &appsv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status:     appsv1alpha1.WorkloadStatus{Phase: "Running"},
	}
}

func TestHandlerRewritesToServicesProxyPath(t *testing.T) {
	const port = int32(8080)

	// Fake API server standing in for the real kube-apiserver: asserts the
	// request path/method it receives matches the services/proxy convention,
	// and that the gateway's own token query param never reaches it.
	var gotPath, gotQuery string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello from proxied service"))
	}))
	defer apiServer.Close()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	if err := appsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: port}}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, readyWorkload()).Build()

	proxy, err := NewServiceProxy(fakeClient, &rest.Config{Host: apiServer.URL}, namespace)
	if err != nil {
		t.Fatalf("NewServiceProxy: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/gw/{name}/{entrypoint}/{subpath...}", proxy.Handler())

	req := httptest.NewRequest(http.MethodGet, "/gw/demo/http/some/path?token=secret-should-be-stripped&foo=bar", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != "hello from proxied service" {
		t.Fatalf("unexpected proxied body: %q", body)
	}

	wantPath := "/api/v1/namespaces/default/services/demo:8080/proxy/some/path"
	if gotPath != wantPath {
		t.Fatalf("expected rewritten path %q, got %q", wantPath, gotPath)
	}
	if gotQuery != "foo=bar" {
		t.Fatalf("expected token stripped from forwarded query, got %q", gotQuery)
	}
}

func TestHandlerReturns404WhenServiceMissing(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	if err := appsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	// A ready Workload with the requested name/namespace but no backing
	// Service — this test exercises the Service-not-found path specifically,
	// which only fires once the Workload is already Running.
	wl := &appsv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: "does-not-exist", Namespace: namespace},
		Status:     appsv1alpha1.WorkloadStatus{Phase: "Running"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wl).Build()

	proxy, err := NewServiceProxy(fakeClient, &rest.Config{Host: testInvalidAPIServerHost}, namespace)
	if err != nil {
		t.Fatalf("NewServiceProxy: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/gw/{name}/{entrypoint}/{subpath...}", proxy.Handler())

	req := httptest.NewRequest(http.MethodGet, "/gw/does-not-exist/http/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandlerReturns404WhenEntrypointUnknown(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	if err := appsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
			{Name: "http", Port: 80},
			{Name: "backoffice", Port: 8080},
		}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, readyWorkload()).Build()

	proxy, err := NewServiceProxy(fakeClient, &rest.Config{Host: testInvalidAPIServerHost}, namespace)
	if err != nil {
		t.Fatalf("NewServiceProxy: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/gw/{name}/{entrypoint}/{subpath...}", proxy.Handler())

	req := httptest.NewRequest(http.MethodGet, "/gw/demo/does-not-exist/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlerReturns404WhenWorkloadMissing(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	if err := appsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	proxy, err := NewServiceProxy(fakeClient, &rest.Config{Host: testInvalidAPIServerHost}, namespace)
	if err != nil {
		t.Fatalf("NewServiceProxy: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/gw/{name}/{entrypoint}/{subpath...}", proxy.Handler())

	req := httptest.NewRequest(http.MethodGet, "/gw/demo/http/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Fatalf("expected plain-text 404 (not the HTML waiting page), got Content-Type %q", ct)
	}
}

func TestHandlerServesWaitingPageWhenWorkloadNotReady(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	if err := appsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	// Spec.Replicas left nil deliberately, to exercise the defaultReplicas
	// fallback in the "X/Y replicas ready" copy.
	wl := &appsv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status:     appsv1alpha1.WorkloadStatus{Phase: "Deploying", ReadyReplicas: 0},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wl).Build()

	proxy, err := NewServiceProxy(fakeClient, &rest.Config{Host: testInvalidAPIServerHost}, namespace)
	if err != nil {
		t.Fatalf("NewServiceProxy: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/gw/{name}/{entrypoint}/{subpath...}", proxy.Handler())

	req := httptest.NewRequest(http.MethodGet, "/gw/demo/http/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("expected text/html Content-Type, got %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<meta http-equiv="refresh"`) {
		t.Fatalf("expected self-refreshing meta tag in waiting page, got: %s", body)
	}
	if !strings.Contains(body, name) {
		t.Fatalf("expected workload name %q in waiting page, got: %s", name, body)
	}
}

func TestHandlerServesFailedPageWhenWorkloadFailed(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	if err := appsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	const failureMessage = "reconciling deployment: some underlying error"
	wl := &appsv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: appsv1alpha1.WorkloadStatus{
			Phase: "Failed",
			Conditions: []metav1.Condition{{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				Reason:             "Error",
				Message:            failureMessage,
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wl).Build()

	proxy, err := NewServiceProxy(fakeClient, &rest.Config{Host: testInvalidAPIServerHost}, namespace)
	if err != nil {
		t.Fatalf("NewServiceProxy: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/gw/{name}/{entrypoint}/{subpath...}", proxy.Handler())

	req := httptest.NewRequest(http.MethodGet, "/gw/demo/http/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, failureMessage) {
		t.Fatalf("expected failure message %q in error page, got: %s", failureMessage, body)
	}
	// Still self-refreshing: setFailed's requeue means Failed isn't terminal,
	// so a transient failure can recover on its own — see plan notes.
	if !strings.Contains(body, `<meta http-equiv="refresh"`) {
		t.Fatalf("expected self-refreshing meta tag even on the failed page, got: %s", body)
	}
}

// TestNewServiceProxyForcesHTTP1 guards against a real production incident:
// the API server's services/proxy subresource resets the connection
// mid-response over HTTP/2 for anything beyond small/simple payloads
// (observed as "stream error: ...; INTERNAL_ERROR; received from peer" on
// every request once a proxied workload's response got large enough — a
// tiny demo page didn't trigger it, a full editor UI's static assets did).
// The TLS server here has HTTP/2 explicitly enabled so a client with no
// NextProtos override would negotiate it — proving this is actually
// NewServiceProxy's override taking effect, not just an absence of h2 on
// the server side.
func TestNewServiceProxyForcesHTTP1(t *testing.T) {
	var negotiatedProto string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		negotiatedProto = r.Proto
		w.WriteHeader(http.StatusOK)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	fakeClient := fake.NewClientBuilder().Build()
	proxy, err := NewServiceProxy(fakeClient, &rest.Config{
		Host:            server.URL,
		TLSClientConfig: rest.TLSClientConfig{Insecure: true},
	}, namespace)
	if err != nil {
		t.Fatalf("NewServiceProxy: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := proxy.transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if negotiatedProto != "HTTP/1.1" {
		t.Fatalf("expected NewServiceProxy's transport to force HTTP/1.1 against an HTTP/2-capable server, got %q", negotiatedProto)
	}
}

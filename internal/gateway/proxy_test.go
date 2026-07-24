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
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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

// backendService builds a fake Service whose entrypoint port(s) resolve to
// backendURL's actual host:port — the only way a fake client.Client's
// Service object becomes an address ServiceProxy can really dial, now that
// it proxies directly to a Service's ClusterIP:port instead of routing
// through the kube-apiserver (see proxy.go's doc comment for why).
// portName defaults to "http" if empty.
func backendService(t *testing.T, backendURL, portName string) *corev1.Service {
	t.Helper()
	if portName == "" {
		portName = schemeHTTP
	}
	u, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("parsing backend URL %q: %v", backendURL, err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("splitting backend host:port %q: %v", u.Host, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parsing backend port %q: %v", portStr, err)
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			ClusterIP: host,
			Ports:     []corev1.ServicePort{{Name: portName, Port: int32(port)}},
		},
	}
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	if err := appsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	return scheme
}

func TestHandlerProxiesDirectlyToServiceClusterIP(t *testing.T) {
	// Fake backend standing in for a real workload Pod: asserts the request
	// path/method it receives is a plain, unprefixed path (no
	// api/v1/namespaces/.../proxy rewriting — that mechanism is gone, see
	// proxy.go's doc comment), and that the gateway's own token query param
	// never reaches it.
	var gotPath, gotQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello from proxied service"))
	}))
	defer backend.Close()

	scheme := newTestScheme(t)
	svc := backendService(t, backend.URL, schemeHTTP)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, readyWorkload()).Build()

	proxy := NewServiceProxy(fakeClient, namespace)

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

	if gotPath != "/some/path" {
		t.Fatalf("expected rewritten path %q, got %q", "/some/path", gotPath)
	}
	if gotQuery != "foo=bar" {
		t.Fatalf("expected token stripped from forwarded query, got %q", gotQuery)
	}
}

// TestHandlerStreamsLargeResponseBodyIntact is the regression guard for the
// actual production incident that motivated moving off the kube-apiserver
// services/proxy subresource: that subresource reliably corrupted/truncated
// large response bodies (`httputil: ReverseProxy read error during body
// copy: unexpected EOF`), specifically hit by code-server's editor UI
// assets, that nginx's one-line demo page and firefox/chrome's simpler
// frames never triggered. Proxying straight to the backend removes that
// layer entirely; this pins that a genuinely large (multi-MB) body still
// comes through byte-for-byte via the new path.
func TestHandlerStreamsLargeResponseBodyIntact(t *testing.T) {
	const size = 5 * 1024 * 1024 // 5MB — well past anything a single TCP write/read syscall handles in one shot
	want := bytes.Repeat([]byte("0123456789abcdef"), size/16)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(want)
	}))
	defer backend.Close()

	scheme := newTestScheme(t)
	svc := backendService(t, backend.URL, schemeHTTP)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, readyWorkload()).Build()

	proxy := NewServiceProxy(fakeClient, namespace)
	mux := http.NewServeMux()
	mux.Handle("/gw/{name}/{entrypoint}/{subpath...}", proxy.Handler())

	req := httptest.NewRequest(http.MethodGet, "/gw/demo/http/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	got := rec.Body.Bytes()
	if len(got) != len(want) {
		t.Fatalf("expected %d bytes, got %d (truncated/corrupted body copy)", len(want), len(got))
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("proxied body does not match the original byte-for-byte")
	}
}

// TestHandlerProxiesWebSocketUpgrade guards code-server's other real
// dependency on this gateway (terminal, extension host both need a live
// WebSocket immediately after page load). This specifically exercises the
// httputil.ReverseProxy hijack-and-copy path — needs a real client
// connection, not an httptest.ResponseRecorder (it doesn't implement
// http.Hijacker) — so both the "browser" and the workload backend here are
// real net.Conns. Also guards against a regression the transport swap could
// have silently caused: ReverseProxy.handleUpgradeResponse type-asserts
// the RoundTripper's response body to io.ReadWriteCloser, which a bare
// *http.Transport satisfies for a 101 response but a wrapped/decorated
// RoundTripper (e.g. the client-go transport this package used before)
// might not.
func TestHandlerProxiesWebSocketUpgrade(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("backend ResponseWriter does not support hijacking")
			return
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijacking backend connection: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, _ = bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = bufrw.Flush()

		line, err := bufrw.ReadString('\n')
		if err != nil {
			t.Errorf("backend reading upgraded frame: %v", err)
			return
		}
		_, _ = bufrw.WriteString("echo:" + line)
		_ = bufrw.Flush()
	}))
	defer backend.Close()

	scheme := newTestScheme(t)
	svc := backendService(t, backend.URL, schemeHTTP)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, readyWorkload()).Build()

	proxy := NewServiceProxy(fakeClient, namespace)
	mux := http.NewServeMux()
	mux.Handle("/gw/{name}/{entrypoint}/{subpath...}", proxy.Handler())
	gatewayServer := httptest.NewServer(mux)
	defer gatewayServer.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(gatewayServer.URL, "http://"))
	if err != nil {
		t.Fatalf("dialing gateway: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_, err = conn.Write([]byte("GET /gw/demo/http/ HTTP/1.1\r\n" +
		"Host: gateway\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"))
	if err != nil {
		t.Fatalf("writing upgrade request: %v", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("reading upgrade response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101 Switching Protocols, got %d", resp.StatusCode)
	}

	if _, err := conn.Write([]byte("hello\n")); err != nil {
		t.Fatalf("writing post-upgrade frame: %v", err)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading echoed frame: %v", err)
	}
	if line != "echo:hello\n" {
		t.Fatalf("expected echoed frame %q, got %q", "echo:hello\n", line)
	}
}

// TestHandlerUsesHTTPSForHTTPSNamedEntrypoint guards the scheme-selection
// convention this package still honors even without the kube-apiserver's
// own port-name-based scheme detection: a ServicePort named exactly
// "https" is dialed with TLS (self-signed accepted — see NewServiceProxy's
// doc comment), forced to HTTP/1.1 on the same defensive grounds as the
// package's prior HTTP/2 incident. The server here has HTTP/2 explicitly
// enabled so a client with no override would negotiate it, proving this is
// actually NewServiceProxy's transport config taking effect, not just an
// absence of h2 on the server side.
func TestHandlerUsesHTTPSForHTTPSNamedEntrypoint(t *testing.T) {
	var negotiatedProto string
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		negotiatedProto = r.Proto
		_, _ = w.Write([]byte("ok via tls"))
	}))
	backend.EnableHTTP2 = true
	backend.StartTLS()
	defer backend.Close()

	scheme := newTestScheme(t)
	svc := backendService(t, backend.URL, serviceProxyScheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, readyWorkload()).Build()

	proxy := NewServiceProxy(fakeClient, namespace)
	mux := http.NewServeMux()
	mux.Handle("/gw/{name}/{entrypoint}/{subpath...}", proxy.Handler())

	req := httptest.NewRequest(http.MethodGet, "/gw/demo/"+serviceProxyScheme+"/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if negotiatedProto != "HTTP/1.1" {
		t.Fatalf("expected the backend to see HTTP/1.1, got %q", negotiatedProto)
	}
}

func TestHandlerReturns404WhenServiceMissing(t *testing.T) {
	scheme := newTestScheme(t)
	// A ready Workload with the requested name/namespace but no backing
	// Service — this test exercises the Service-not-found path specifically,
	// which only fires once the Workload is already Running.
	wl := &appsv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: "does-not-exist", Namespace: namespace},
		Status:     appsv1alpha1.WorkloadStatus{Phase: "Running"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wl).Build()

	proxy := NewServiceProxy(fakeClient, namespace)

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
	scheme := newTestScheme(t)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			ClusterIP: "10.0.0.1",
			Ports: []corev1.ServicePort{
				{Name: schemeHTTP, Port: 80},
				{Name: "backoffice", Port: 8080},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, readyWorkload()).Build()

	proxy := NewServiceProxy(fakeClient, namespace)

	mux := http.NewServeMux()
	mux.Handle("/gw/{name}/{entrypoint}/{subpath...}", proxy.Handler())

	req := httptest.NewRequest(http.MethodGet, "/gw/demo/does-not-exist/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandlerReturns502WhenServiceHasNoClusterIP guards the defensive check
// against a headless Service (ClusterIP: None or unset) — not something
// reconcileService produces today, but a real failure mode now that this
// package dials the ClusterIP directly instead of asking the kube-apiserver
// to resolve it.
func TestHandlerReturns502WhenServiceHasNoClusterIP(t *testing.T) {
	scheme := newTestScheme(t)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Ports:     []corev1.ServicePort{{Name: schemeHTTP, Port: 80}},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, readyWorkload()).Build()

	proxy := NewServiceProxy(fakeClient, namespace)

	mux := http.NewServeMux()
	mux.Handle("/gw/{name}/{entrypoint}/{subpath...}", proxy.Handler())

	req := httptest.NewRequest(http.MethodGet, "/gw/demo/http/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlerServesNotFoundPageWhenWorkloadMissing(t *testing.T) {
	scheme := newTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	proxy := NewServiceProxy(fakeClient, namespace)

	mux := http.NewServeMux()
	mux.Handle("/gw/{name}/{entrypoint}/{subpath...}", proxy.Handler())

	req := httptest.NewRequest(http.MethodGet, "/gw/demo/http/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("expected the HTML not-found page, got Content-Type %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "demo") {
		t.Fatalf("expected workload name %q in not-found page, got: %s", "demo", body)
	}
	// A missing Workload won't come back on its own — must not poll like the
	// loading/failed pages do.
	if strings.Contains(body, `<meta http-equiv="refresh"`) {
		t.Fatalf("expected no self-refresh meta tag on the not-found page, got: %s", body)
	}
}

func TestHandlerServesWaitingPageWhenWorkloadNotReady(t *testing.T) {
	scheme := newTestScheme(t)
	wl := &appsv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status:     appsv1alpha1.WorkloadStatus{Phase: "Deploying", ReadyReplicas: 0},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wl).Build()

	proxy := NewServiceProxy(fakeClient, namespace)

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
	scheme := newTestScheme(t)
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

	proxy := NewServiceProxy(fakeClient, namespace)

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

func TestHandlerServesStoppedPageWhenWorkloadStopped(t *testing.T) {
	scheme := newTestScheme(t)
	wl := &appsv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status:     appsv1alpha1.WorkloadStatus{Phase: "Stopped", ReadyReplicas: 0},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wl).Build()

	proxy := NewServiceProxy(fakeClient, namespace)

	mux := http.NewServeMux()
	mux.Handle("/gw/{name}/{entrypoint}/{subpath...}", proxy.Handler())

	req := httptest.NewRequest(http.MethodGet, "/gw/demo/http/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "stopped") {
		t.Fatalf("expected the stopped page, got: %s", body)
	}
	// A Stopped workload won't start on its own the way Deploying/Failed
	// might — this must not be the endlessly-polling loading page.
	if strings.Contains(body, `<meta http-equiv="refresh"`) {
		t.Fatalf("expected no self-refresh meta tag on the stopped page, got: %s", body)
	}
}

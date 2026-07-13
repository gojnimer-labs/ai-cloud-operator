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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestHandlerRewritesToServicesProxyPath(t *testing.T) {
	const (
		namespace = "default"
		name      = "demo"
		port      = int32(8080)
	)

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
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: port}}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build()

	proxy, err := NewServiceProxy(fakeClient, &rest.Config{Host: apiServer.URL})
	if err != nil {
		t.Fatalf("NewServiceProxy: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/gw/{namespace}/{name}/{subpath...}", proxy.Handler())

	req := httptest.NewRequest(http.MethodGet, "/gw/default/demo/some/path?token=secret-should-be-stripped&foo=bar", nil)
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
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	proxy, err := NewServiceProxy(fakeClient, &rest.Config{Host: "http://example.invalid"})
	if err != nil {
		t.Fatalf("NewServiceProxy: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/gw/{namespace}/{name}/{subpath...}", proxy.Handler())

	req := httptest.NewRequest(http.MethodGet, "/gw/default/does-not-exist/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

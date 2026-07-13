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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	"github.com/gojnimer-labs/ai-cloud-operator/internal/gateway"
)

const (
	testDeployToken   = "deploy-token-1"
	testGatewaySecret = "gateway-signing-secret-1"
	testServiceName   = "gw-demo"
	testServiceNS     = "default"
	testServicePort   = int32(8080)
)

func newTestServer(t *testing.T) (*Server, client.Client) {
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
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: testServicePort}}},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1alpha1.Workload{}).
		WithObjects(svc).
		Build()

	proxy, err := gateway.NewServiceProxy(c, &rest.Config{Host: "http://127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServiceProxy: %v", err)
	}

	s := New(c, ":0", func() string { return testDeployToken }, []byte(testGatewaySecret), proxy)
	return s, c
}

func (s *Server) testHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.Handle("POST /workloads", s.requireDeployToken(http.HandlerFunc(s.handleDeploy)))
	mux.Handle("GET /workloads/{namespace}/{name}", s.requireDeployToken(http.HandlerFunc(s.handleGet)))
	mux.Handle("DELETE /workloads/{namespace}/{name}", s.requireDeployToken(http.HandlerFunc(s.handleDelete)))
	mux.Handle("GET /gw/{namespace}/{name}/{subpath...}", s.requireGatewayToken(s.proxy.Handler()))
	return mux
}

func TestHealthzIsUnauthenticated(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestWorkloadsRequiresBearerToken(t *testing.T) {
	s, _ := newTestServer(t)

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
			req := httptest.NewRequest(http.MethodGet, "/workloads/default/demo", nil)
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
	s, c := newTestServer(t)

	body, _ := json.Marshal(deployRequest{
		Name:          "demo",
		Namespace:     "default",
		Image:         "nginx:latest",
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
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "demo"}, &workload); err != nil {
		t.Fatalf("expected workload CR to be created: %v", err)
	}
	if workload.Spec.Image != "nginx:latest" {
		t.Fatalf("expected image nginx:latest, got %q", workload.Spec.Image)
	}
}

func TestDeployRejectsMissingFields(t *testing.T) {
	s, _ := newTestServer(t)

	body, _ := json.Marshal(deployRequest{Name: "demo"})
	req := httptest.NewRequest(http.MethodPost, "/workloads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDeployToken)
	rec := httptest.NewRecorder()

	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestGetReturns404ForUnknownWorkload(t *testing.T) {
	s, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/workloads/default/does-not-exist", nil)
	req.Header.Set("Authorization", "Bearer "+testDeployToken)
	rec := httptest.NewRecorder()

	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestDeleteRemovesWorkloadCR(t *testing.T) {
	s, c := newTestServer(t)
	ctx := context.Background()

	workload := &appsv1alpha1.Workload{}
	workload.Name = "demo"
	workload.Namespace = "default"
	workload.Spec.Image = "nginx:latest"
	if err := c.Create(ctx, workload); err != nil {
		t.Fatalf("seeding workload: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/workloads/default/demo", nil)
	req.Header.Set("Authorization", "Bearer "+testDeployToken)
	rec := httptest.NewRecorder()

	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}

	var check appsv1alpha1.Workload
	err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "demo"}, &check)
	if err == nil {
		t.Fatalf("expected workload to be deleted")
	}
}

func mintTestGatewayToken(t *testing.T, secret, namespace, name string, exp time.Time) string {
	t.Helper()
	raw, err := json.Marshal(gateway.Payload{Namespace: namespace, Name: name, Exp: exp.Unix()})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payloadB64))
	sigB64 := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payloadB64 + "." + sigB64
}

func TestGatewayRequiresToken(t *testing.T) {
	s, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/gw/"+testServiceNS+"/"+testServiceName+"/", nil)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no token, got %d", rec.Code)
	}
}

func TestGatewayRejectsWrongScopeToken(t *testing.T) {
	s, _ := newTestServer(t)

	// Token minted for a different workload name must not authorize this one.
	token := mintTestGatewayToken(t, testGatewaySecret, testServiceNS, "some-other-workload", time.Now().Add(time.Minute))
	req := httptest.NewRequest(http.MethodGet, "/gw/"+testServiceNS+"/"+testServiceName+"/?token="+token, nil)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong-scope token, got %d", rec.Code)
	}
}

func TestGatewayRejectsExpiredToken(t *testing.T) {
	s, _ := newTestServer(t)

	token := mintTestGatewayToken(t, testGatewaySecret, testServiceNS, testServiceName, time.Now().Add(-time.Hour))
	req := httptest.NewRequest(http.MethodGet, "/gw/"+testServiceNS+"/"+testServiceName+"/?token="+token, nil)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired token, got %d", rec.Code)
	}
}

func TestGatewayAcceptsValidTokenAndProxies(t *testing.T) {
	// Stand in for the real kube-apiserver so we can assert the proxied
	// request actually reaches it once the gateway token passes.
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/v1/namespaces/" + testServiceNS + "/services/" + testServiceName + ":8080/proxy/"
		if r.URL.Path != wantPath {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		w.Write([]byte("ok from service"))
	}))
	defer apiServer.Close()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: testServiceNS},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: testServicePort}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build()
	proxy, err := gateway.NewServiceProxy(c, &rest.Config{Host: apiServer.URL})
	if err != nil {
		t.Fatalf("NewServiceProxy: %v", err)
	}
	s := New(c, ":0", func() string { return testDeployToken }, []byte(testGatewaySecret), proxy)

	token := mintTestGatewayToken(t, testGatewaySecret, testServiceNS, testServiceName, time.Now().Add(time.Minute))
	req := httptest.NewRequest(http.MethodGet, "/gw/"+testServiceNS+"/"+testServiceName+"/?token="+token, nil)
	rec := httptest.NewRecorder()
	s.testHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "ok from service" {
		t.Fatalf("unexpected proxied body: %q", rec.Body.String())
	}
}

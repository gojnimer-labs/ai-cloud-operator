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

// Package api exposes the inbound HTTP API Convex calls to deploy, delete,
// and inspect Workload custom resources. It never touches the Deployment or
// Service directly — the Workload CRD remains the sole source of truth, and
// internal/controller.WorkloadReconciler does the actual reconciliation.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/gojnimer-labs/ai-cloud-operator/api/v1alpha1"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/gateway"
)

// TokenGetter returns the deploy token currently expected from callers.
// Backed by convexclient.Runnable.CurrentDeployToken so the API always
// checks against the freshest token, even after a re-registration rotates it.
type TokenGetter func() string

// Server is the inbound HTTP API. It serves two independently-authenticated
// route groups on one port: the Convex-only /workloads* management API
// (deploy token) and the browser-facing /gw/* gateway proxy (short-lived
// signed access tokens). It implements controller-runtime's manager.Runnable.
type Server struct {
	client        client.Client
	listenAddr    string
	token         TokenGetter
	gatewaySecret []byte
	proxy         *gateway.ServiceProxy

	httpServer *http.Server
}

// New builds a Server listening on listenAddr, using c to read/write
// Workload custom resources, token to authenticate Convex's management
// calls, and gatewaySecret/proxy to authenticate and serve end-user gateway
// requests.
func New(c client.Client, listenAddr string, token TokenGetter, gatewaySecret []byte, proxy *gateway.ServiceProxy) *Server {
	return &Server{client: c, listenAddr: listenAddr, token: token, gatewaySecret: gatewaySecret, proxy: proxy}
}

// Start implements manager.Runnable. It blocks until ctx is cancelled, then
// gracefully shuts the HTTP server down.
func (s *Server) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("api")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.Handle("POST /workloads", s.requireDeployToken(http.HandlerFunc(s.handleDeploy)))
	mux.Handle("GET /workloads/{namespace}/{name}", s.requireDeployToken(http.HandlerFunc(s.handleGet)))
	mux.Handle("DELETE /workloads/{namespace}/{name}", s.requireDeployToken(http.HandlerFunc(s.handleDelete)))
	mux.Handle("GET /gw/{namespace}/{name}/{subpath...}", s.requireGatewayToken(s.proxy.Handler()))

	s.httpServer = &http.Server{
		Addr:              s.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("starting operator api", "addr", s.listenAddr)
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable — the API
// must keep serving even on non-leader replicas in a future HA setup.
func (s *Server) NeedLeaderElection() bool {
	return false
}

// requireDeployToken guards the Convex-only /workloads* management API with
// "Authorization: Bearer <deployToken>".
func (s *Server) requireDeployToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		presented := header[len(prefix):]

		expected := s.token()
		if expected == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 {
			http.Error(w, "invalid bearer token", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// requireGatewayToken guards the browser-facing /gw/* routes with a
// short-lived, per-workload signed token carried as ?token=... — a browser
// reaching this route via top-level navigation (a new tab) can't attach a
// custom Authorization header, so the token must ride in the URL.
func (s *Server) requireGatewayToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if token == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		namespace, name := r.PathValue("namespace"), r.PathValue("name")
		if _, err := gateway.Verify(s.gatewaySecret, namespace, name, token); err != nil {
			http.Error(w, "invalid or expired token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

type deployRequest struct {
	Name          string          `json:"name"`
	Namespace     string          `json:"namespace"`
	Image         string          `json:"image"`
	Replicas      *int32          `json:"replicas,omitempty"`
	ContainerPort int32           `json:"containerPort,omitempty"`
	Env           []corev1.EnvVar `json:"env,omitempty"`
	Subdomain     string          `json:"subdomain,omitempty"`
	UserID        string          `json:"userId,omitempty"`
	Config        map[string]any  `json:"config,omitempty"`
}

type workloadResponse struct {
	Name      string                      `json:"name"`
	Namespace string                      `json:"namespace"`
	Status    appsv1alpha1.WorkloadStatus `json:"status"`
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var req deployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Namespace == "" || req.Image == "" {
		http.Error(w, "name, namespace, and image are required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	workload := &appsv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, s.client, workload, func() error {
		workload.Spec.Image = req.Image
		workload.Spec.Replicas = req.Replicas
		workload.Spec.ContainerPort = req.ContainerPort
		workload.Spec.Env = req.Env
		workload.Spec.Subdomain = req.Subdomain
		workload.Spec.UserID = req.UserID
		if req.Config != nil {
			raw, marshalErr := json.Marshal(req.Config)
			if marshalErr != nil {
				return marshalErr
			}
			workload.Spec.Config = &apiextensionsv1.JSON{Raw: raw}
		}
		return nil
	})
	if err != nil {
		http.Error(w, "failed to apply workload: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(workloadResponse{
		Name:      workload.Name,
		Namespace: workload.Namespace,
		Status:    workload.Status,
	})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	name := r.PathValue("name")

	var workload appsv1alpha1.Workload
	if err := s.client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &workload); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, "workload not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to get workload: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(workloadResponse{
		Name:      workload.Name,
		Namespace: workload.Namespace,
		Status:    workload.Status,
	})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	name := r.PathValue("name")

	workload := &appsv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	if err := s.client.Delete(r.Context(), workload); err != nil {
		if apierrors.IsNotFound(err) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "failed to delete workload: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

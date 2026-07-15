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
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/gojnimer-labs/ai-cloud-operator/api/v1alpha1"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/catalog"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/gateway"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/podexec"
)

// TokenGetter returns the deploy token currently expected from callers.
// Backed by convexclient.Runnable.CurrentDeployToken so the API always
// checks against the freshest token, even after a re-registration rotates it.
type TokenGetter func() string

// GatewayVerifier asks Convex to check and consume a one-time gateway access
// token minted for namespace/name, returning its userId on success. Convex
// is the only party that can enforce true single-use (it holds the state);
// see internal/convexclient.Runnable.VerifyGatewayToken.
type GatewayVerifier interface {
	VerifyGatewayToken(ctx context.Context, token, namespace, name string) (userID string, err error)
}

const (
	// gatewayCookieName carries the operator's own signed session — see
	// requireGatewayToken. One name reused across every workload; Path
	// scoping (plus the signed namespace/name inside) keeps them distinct.
	gatewayCookieName = "gw_auth"
	// gatewayCookieTTL is deliberately much longer than the one-time token's
	// TTL (see ai-cloud-v2/convex/gateway/mutations.ts) — this cookie is the
	// session for actually using the workload, not just the handoff.
	gatewayCookieTTL = 30 * time.Minute
)

// Server is the inbound HTTP API. It serves two independently-authenticated
// route groups on one port: the Convex-only /workloads* management API
// (deploy token) and the browser-facing /gw/* gateway proxy (one-time token
// exchanged for a signed session cookie — see requireGatewayToken). It
// implements controller-runtime's manager.Runnable.
type Server struct {
	client          client.Client
	listenAddr      string
	token           TokenGetter
	gatewaySecret   []byte
	gatewayVerifier GatewayVerifier
	proxy           *gateway.ServiceProxy
	podExecutor     catalog.PodExecutor

	httpServer *http.Server
}

// New builds a Server listening on listenAddr, using c to read/write
// Workload custom resources, token to authenticate Convex's management
// calls, gatewaySecret/verifier/proxy to authenticate and serve end-user
// gateway requests, and podExecutor to run a workload's Operations (see
// handleRunFunction).
func New(c client.Client, listenAddr string, token TokenGetter, gatewaySecret []byte, verifier GatewayVerifier, proxy *gateway.ServiceProxy, podExecutor catalog.PodExecutor) *Server {
	return &Server{client: c, listenAddr: listenAddr, token: token, gatewaySecret: gatewaySecret, gatewayVerifier: verifier, proxy: proxy, podExecutor: podExecutor}
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
	mux.Handle("GET /gw/{namespace}/{name}/{entrypoint}/{subpath...}", s.requireGatewayToken(s.proxy.Handler()))
	mux.Handle("GET /catalog", s.requireDeployToken(http.HandlerFunc(s.handleCatalog)))
	mux.Handle("POST /workloads/{namespace}/{name}/functions/{key}", s.requireDeployToken(http.HandlerFunc(s.handleRunFunction)))

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

// requireGatewayToken guards the browser-facing /gw/* routes. Two paths:
//
//   - Fast path: a valid signed session cookie from a prior exchange is
//     present — verified entirely locally, no Convex call. This is what
//     makes sub-resource requests work (a proxied app's own HTML/JS loading
//     its assets from other /gw/... URLs that never saw the original
//     one-time token).
//   - Exchange path: no cookie (or an invalid one), so this must be a fresh
//     one-time token from Convex (?token=... — a browser reaching this
//     route via top-level navigation/new-tab can't attach a custom
//     Authorization header, so the token has to ride in the URL). The
//     operator hands it to Convex to verify+consume (the only party that
//     can enforce true single-use), and on success mints its own session
//     cookie scoped to this exact workload so every later request —
//     including sub-resource ones — authenticates from the cookie alone.
func (s *Server) requireGatewayToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		namespace, name := r.PathValue("namespace"), r.PathValue("name")

		if cookie, err := r.Cookie(gatewayCookieName); err == nil {
			if _, err := gateway.Verify(s.gatewaySecret, namespace, name, cookie.Value); err == nil {
				next.ServeHTTP(w, r)
				return
			}
		}

		token := r.URL.Query().Get("token")
		if token == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		userID, err := s.gatewayVerifier.VerifyGatewayToken(r.Context(), token, namespace, name)
		if err != nil {
			http.Error(w, "invalid or expired token", http.StatusUnauthorized)
			return
		}

		signed, err := gateway.Sign(s.gatewaySecret, gateway.Payload{
			Namespace: namespace,
			Name:      name,
			UserID:    userID,
			Exp:       time.Now().Add(gatewayCookieTTL).Unix(),
		})
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     gatewayCookieName,
			Value:    signed,
			Path:     "/gw/" + namespace + "/" + name,
			MaxAge:   int(gatewayCookieTTL.Seconds()),
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
		next.ServeHTTP(w, r)
	})
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// handleCatalog returns the full template registry, including
// system-sourced parameters (e.g. profileDownloadUrl) — Convex needs to know
// those keys exist so it can compute and inject them, and the frontend is
// expected to only render dataSource.kind:"static"/"dynamic" parameters as
// form fields, never "system" ones.
func (s *Server) handleCatalog(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(catalog.List())
}

type runFunctionRequest struct {
	Params map[string]any `json:"params,omitempty"`
}

type runFunctionResponse struct {
	AdditionalInfo []catalog.AdditionalInfo `json:"additionalInfo"`
}

// handleRunFunction invokes a named Operation (see catalog.Template's
// Operations) against a workload's currently-running pod — the generic
// invocation path any operation reuses, not just backup_state: look up the
// workload's template, find the operation by key, resolve its parameters
// the same way deploy-time parameters are resolved, find a running pod, and
// hand off to the operation's own Run implementation.
func (s *Server) handleRunFunction(w http.ResponseWriter, r *http.Request) {
	log := logf.FromContext(r.Context())
	namespace := r.PathValue("namespace")
	name := r.PathValue("name")
	key := r.PathValue("key")

	var req runFunctionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	var workload appsv1alpha1.Workload
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &workload); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, "workload not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to get workload: "+err.Error(), http.StatusInternalServerError)
		return
	}

	tmpl, ok := catalog.Get(workload.Spec.TemplateName)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown template %q", workload.Spec.TemplateName), http.StatusBadRequest)
		return
	}
	fn, ok := catalog.GetOperation(tmpl, key)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown function %q for template %q", key, tmpl.ID), http.StatusNotFound)
		return
	}

	resolvedParams, err := catalog.ResolveParams(fn.Parameters, req.Params)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	podName, err := podexec.FindPod(ctx, s.client, namespace, name)
	if err != nil {
		log.Info("handleRunFunction: no running pod", "namespace", namespace, "name", name, "function", key, "error", err.Error())
		http.Error(w, "workload has no running pod: "+err.Error(), http.StatusConflict)
		return
	}

	result, err := fn.Run(ctx, s.podExecutor, catalog.PodRef{Namespace: namespace, PodName: podName}, resolvedParams)
	if err != nil {
		log.Error(err, "handleRunFunction: function failed", "namespace", namespace, "name", name, "function", key)
		http.Error(w, "function failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(runFunctionResponse{AdditionalInfo: result})
}

type deployRequest struct {
	Name          string          `json:"name"`
	Namespace     string          `json:"namespace"`
	TemplateName  string          `json:"templateName,omitempty"`
	Image         string          `json:"image,omitempty"`
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
	log := logf.FromContext(r.Context())

	var req deployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Error(err, "handleDeploy: invalid json body")
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	// Deliberately not logging req.Config: a system-sourced parameter (e.g.
	// profileDownloadUrl) can be a presigned URL with an auth signature
	// baked into its query string — logging it in plaintext would leak
	// that credential into the operator's own logs.
	log.Info("handleDeploy: received request",
		"name", req.Name, "namespace", req.Namespace, "templateName", req.TemplateName)

	if req.Name == "" || req.Namespace == "" {
		http.Error(w, "name and namespace are required", http.StatusBadRequest)
		return
	}
	if errs := validation.IsDNS1123Subdomain(req.Name); len(errs) > 0 {
		log.Info("handleDeploy: invalid name", "name", req.Name, "errors", errs)
		http.Error(w, fmt.Sprintf("invalid name %q: %s", req.Name, strings.Join(errs, "; ")), http.StatusBadRequest)
		return
	}
	if req.TemplateName == "" && req.Image == "" {
		http.Error(w, "image is required when templateName is unset", http.StatusBadRequest)
		return
	}
	if req.TemplateName != "" {
		tmpl, ok := catalog.Get(req.TemplateName)
		if !ok {
			log.Info("handleDeploy: unknown template", "templateName", req.TemplateName)
			http.Error(w, fmt.Sprintf("unknown template %q", req.TemplateName), http.StatusBadRequest)
			return
		}
		if _, err := catalog.ResolveParams(tmpl.Parameters, req.Config); err != nil {
			log.Error(err, "handleDeploy: resolving template parameters failed", "templateName", req.TemplateName)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	ctx := r.Context()
	workload := &appsv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, s.client, workload, func() error {
		workload.Spec.TemplateName = req.TemplateName
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
		log.Error(err, "handleDeploy: failed to apply workload", "name", req.Name, "namespace", req.Namespace)
		http.Error(w, "failed to apply workload: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Info("handleDeploy: workload applied", "name", workload.Name, "namespace", workload.Namespace)

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

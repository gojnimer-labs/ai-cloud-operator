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

// +kubebuilder:rbac:groups="",resources=services/proxy,verbs=get;create;update;delete

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/gojnimer-labs/ai-cloud-operator/api/v1alpha1"
)

// Phase/condition values mirror internal/controller/workload_controller.go's
// own unexported consts. Duplicated rather than imported: that package
// already keeps them local instead of exporting them, and importing it from
// here would invert the controller -> gateway dependency direction wired in
// cmd/main.go.
const (
	phaseRunning              = "Running"
	phaseFailed               = "Failed"
	conditionTypeReady        = "Ready"
	defaultReplicas           = int32(1)
	waitingPageRefreshSeconds = 3
)

// ServiceProxy relays HTTP requests into a cluster-internal Service via the
// Kubernetes API server's services/proxy subresource — the same mechanism
// `kubectl proxy`/the Dashboard use. The operator therefore never needs
// direct pod-network reachability to expose a workload.
type ServiceProxy struct {
	k8sClient client.Client
	transport http.RoundTripper
	apiURL    *url.URL
	// namespace is the single, fixed WORKLOAD_NAMESPACE every workload this
	// operator manages lives in — an install-time config value, not
	// something the caller sends per-request (see internal/api.Server).
	namespace string
}

// NewServiceProxy builds a ServiceProxy that authenticates to the API server
// using cfg — the same *rest.Config the manager already uses for every other
// client-go call, so TLS/auth is handled identically.
func NewServiceProxy(k8sClient client.Client, cfg *rest.Config, namespace string) (*ServiceProxy, error) {
	transport, err := rest.TransportFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("building api-server transport: %w", err)
	}
	apiURL, err := url.Parse(cfg.Host)
	if err != nil {
		return nil, fmt.Errorf("parsing api server host %q: %w", cfg.Host, err)
	}
	return &ServiceProxy{k8sClient: k8sClient, transport: transport, apiURL: apiURL, namespace: namespace}, nil
}

// Handler proxies requests for {name}/{entrypoint}/{subpath} through the API
// server's services/proxy subresource, always against p.namespace — the
// single fixed WORKLOAD_NAMESPACE this operator instance manages. The target
// Service's port is resolved with a live Get (not from the Workload CR's
// spec) — this doubles as an existence check (404 if the Service is gone)
// and avoids drift between the CR spec and actual cluster state. entrypoint
// selects which of the Service's (possibly several) named ports to route to
// — it matches a catalog.Entrypoint.Name, which by construction equals a
// real corev1.ServicePort.Name (see catalog.TestEntrypointsMatchRenderedServicePorts).
func (p *ServiceProxy) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		entrypoint := r.PathValue("entrypoint")
		subpath := r.PathValue("subpath")

		log := logf.FromContext(r.Context()).WithName("gateway")

		var workload appsv1alpha1.Workload
		if err := p.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: p.namespace, Name: name}, &workload); err != nil {
			if apierrors.IsNotFound(err) {
				http.Error(w, "workload not found", http.StatusNotFound)
				return
			}
			log.Error(err, "resolving workload")
			http.Error(w, "failed to resolve workload", http.StatusBadGateway)
			return
		}

		if workload.Status.Phase != phaseRunning {
			desired := defaultReplicas
			if workload.Spec.Replicas != nil {
				desired = *workload.Spec.Replicas
			}
			var message string
			if cond := apimeta.FindStatusCondition(workload.Status.Conditions, conditionTypeReady); cond != nil {
				message = cond.Message
			}
			failed := workload.Status.Phase == phaseFailed
			status := http.StatusOK
			if failed {
				status = http.StatusServiceUnavailable
			}
			renderWaitingPage(w, r, status, waitingPageData{
				Namespace:       p.namespace,
				Name:            name,
				Phase:           workload.Status.Phase,
				Message:         message,
				ReadyReplicas:   workload.Status.ReadyReplicas,
				DesiredReplicas: desired,
				RefreshSeconds:  waitingPageRefreshSeconds,
				ShowRefresh:     true, // keep refreshing even on Failed — see plan notes
				Failed:          failed,
			})
			return
		}

		var svc corev1.Service
		if err := p.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: p.namespace, Name: name}, &svc); err != nil {
			if apierrors.IsNotFound(err) {
				http.Error(w, "workload service not found", http.StatusNotFound)
				return
			}
			log.Error(err, "resolving target service")
			http.Error(w, "failed to resolve workload service", http.StatusBadGateway)
			return
		}
		if len(svc.Spec.Ports) == 0 {
			http.Error(w, "workload service exposes no ports", http.StatusBadGateway)
			return
		}
		var port int32
		var found bool
		for _, sp := range svc.Spec.Ports {
			if sp.Name == entrypoint {
				port, found = sp.Port, true
				break
			}
		}
		if !found {
			http.Error(w, "unknown entrypoint", http.StatusNotFound)
			return
		}

		targetPath := fmt.Sprintf("/api/v1/namespaces/%s/services/%s:%d/proxy/%s", p.namespace, name, port, subpath)

		proxy := &httputil.ReverseProxy{
			Transport: p.transport,
			Director: func(req *http.Request) {
				q := req.URL.Query()
				q.Del("token") // never forward our own gateway token upstream
				req.URL.Scheme = p.apiURL.Scheme
				req.URL.Host = p.apiURL.Host
				req.URL.Path = targetPath
				req.URL.RawQuery = q.Encode()
				req.Host = p.apiURL.Host
			},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				log.Error(err, "proxying to workload service")
				http.Error(w, "upstream proxy error", http.StatusBadGateway)
			},
		}
		proxy.ServeHTTP(w, r)
	}
}

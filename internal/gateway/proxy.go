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
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httputil"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/gojnimer-labs/ai-cloud-operator/api/v1alpha1"
)

// Phase/condition values come from api/v1alpha1 — the CRD's own source of
// truth (see PhaseRunning et al.'s doc comments) — so internal/controller
// (which sets them) and this package (which only reads them) share one
// definition without either importing the other.
const gatewayPageRefreshSeconds = 3

// serviceProxyScheme is the corev1.ServicePort.Name a template opts into to
// mark its entrypoint as TLS-backed — same convention every template in
// internal/catalog already follows for its *container* port names
// (portNameHTTPS = "https"). No template uses it today (code-server's own
// investigation found linuxserver/code-server actually speaks plain HTTP on
// its port), but the convention is kept for any future template whose
// backend genuinely does terminate TLS itself.
const serviceProxyScheme = "https"

// schemeHTTP is every other entrypoint's scheme — the default unless a
// ServicePort is named exactly serviceProxyScheme.
const schemeHTTP = "http"

// ServiceProxy relays HTTP requests directly into a cluster-internal
// Service's ClusterIP — ordinary in-cluster networking, the same path any
// other pod in the cluster would use, not the Kubernetes API server's
// services/proxy subresource. This package used to go through that
// subresource (see git history) specifically so the operator would never
// need direct pod-network reachability to expose a workload.
//
// That subresource is what's behind this package's history of code-server
// specifically hitting trouble here: first an HTTP/2 stream reset on large
// responses (fixed by forcing HTTP/1.1, see the old NewServiceProxy's
// history), then later a live deploy logging `httputil: ReverseProxy read
// error during body copy: unexpected EOF` serving the same editor UI's
// static assets. Direct proxying removes that whole subresource hop, which
// is reason enough on its own (fewer moving parts, no api-server RBAC just
// to reach a Service, and it's the standard way a Kubernetes-native
// gateway is built) — but be precise about what's actually confirmed vs.
// suspected for the EOF specifically: a live A/B (2026-07-23) fetching
// code-server's real ~17MB workbench.js through the apiserver subresource
// over HTTP/1.1 — both a single fetch and 10 concurrent fetches — came back
// byte-identical (sha256-verified) to fetching it straight from the pod
// IP. That does NOT reproduce the EOF, so this rewrite is not a confirmed
// fix for that exact incident — the trigger might have been specific to
// the *old* transport (client-go's rest.TransportFor-built one, which this
// package no longer uses at all) rather than the subresource itself, e.g.
// its own connection-pooling/reuse behavior, or something concurrency- or
// session-shaped that a scripted curl fetch doesn't reproduce. What direct
// proxying does concretely fix, independent of that unresolved question:
// it removes a class of risk around httputil.ReverseProxy's WebSocket
// hijack path (handleUpgradeResponse type-asserts the RoundTripper's
// response body to io.ReadWriteCloser, which a decorated/wrapped
// RoundTripper — like client-go's — isn't guaranteed to preserve; a bare
// *http.Transport is, see TestHandlerProxiesWebSocketUpgrade), something
// code-server depends on immediately after page load for its terminal and
// extension host. Verify against a real browser session once this is
// redeployed rather than trusting this comment alone.
//
// This does mean the operator now needs real pod-network reachability
// into WORKLOAD_NAMESPACE — true by default on a flat cluster network
// (confirmed live: no NetworkPolicy exists in either namespace as of this
// change, and a direct curl from a pod in the operator's own namespace to
// a workload Service's ClusterIP DNS name succeeded); if a NetworkPolicy
// is later added restricting egress from the operator's namespace or
// ingress into the workload namespace, it must allow this path.
type ServiceProxy struct {
	k8sClient client.Client
	transport http.RoundTripper
	// namespace is the single, fixed WORKLOAD_NAMESPACE every workload this
	// operator manages lives in — an install-time config value, not
	// something the caller sends per-request (see internal/api.Server).
	namespace string
}

// NewServiceProxy builds a ServiceProxy. The transport talks directly to
// workload Services over plain in-cluster networking — no Kubernetes API
// server authentication involved, unlike the services/proxy-subresource
// approach this replaced, since a normal network connection to a
// ClusterIP needs none. TLSClientConfig only matters for the rare
// serviceProxyScheme("https")-named entrypoint: certs on an in-cluster
// backend are almost always self-signed, hence InsecureSkipVerify, and
// NextProtos is pinned to HTTP/1.1 on the same defensive grounds as this
// package's prior HTTP/2 incident (see ServiceProxy's doc comment) — no
// backend here is known to need h2c/h2, so there's nothing to lose by
// ruling it out up front. Proxy is explicitly nil so this never
// accidentally honors an HTTP_PROXY-style env var and routes workload
// traffic somewhere unexpected — every target here is always a
// same-cluster ClusterIP.
func NewServiceProxy(k8sClient client.Client, namespace string) *ServiceProxy {
	// Cloned from DefaultTransport rather than built from a zero value — a
	// bare &http.Transport{} drops DefaultTransport's dial timeout and idle
	// connection reuse, so a dead ClusterIP would hang on the OS's own TCP
	// timeout instead of failing fast, and every request would pay a fresh
	// TCP handshake. No overall response/idle timeout is added on top: a
	// long-lived WebSocket (code-server's terminal, extension host) must
	// not get killed by one.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // always a same-cluster ClusterIP — never honor an ambient HTTP_PROXY-style env var
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // in-cluster backend, no external CA to verify against
		NextProtos:         []string{"http/1.1"},
	}
	// DefaultTransport also carries ForceAttemptHTTP2 plus a pre-wired
	// TLSNextProto upgrade handler, which negotiates h2 over ALPN
	// regardless of TLSClientConfig.NextProtos above — Clone() copies both.
	// An empty, non-nil TLSNextProto map is the documented way to actually
	// disable that (see http.Transport.TLSNextProto's doc comment); setting
	// NextProtos alone was not sufficient here (caught by
	// TestHandlerUsesHTTPSForHTTPSNamedEntrypoint negotiating HTTP/2 anyway
	// before this line was added).
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	return &ServiceProxy{k8sClient: k8sClient, transport: transport, namespace: namespace}
}

// Handler proxies requests for {name}/{entrypoint}/{subpath} directly to the
// target Service's ClusterIP, always against p.namespace — the single fixed
// WORKLOAD_NAMESPACE this operator instance manages. The target Service is
// resolved with a live Get (not from the Workload CR's spec) — this doubles
// as an existence check (404 if the Service is gone) and avoids drift
// between the CR spec and actual cluster state. entrypoint selects which of
// the Service's (possibly several) named ports to route to — it matches a
// catalog.Entrypoint.Name, which by construction equals a real
// corev1.ServicePort.Name (see catalog.TestEntrypointsMatchRenderedServicePorts).
func (p *ServiceProxy) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		entrypoint := r.PathValue("entrypoint")
		subpath := r.PathValue("subpath")

		log := logf.FromContext(r.Context()).WithName("gateway")

		var workload appsv1alpha1.Workload
		if err := p.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: p.namespace, Name: name}, &workload); err != nil {
			if apierrors.IsNotFound(err) {
				renderNotFoundPage(w, r, name)
				return
			}
			log.Error(err, "resolving workload")
			http.Error(w, "failed to resolve workload", http.StatusBadGateway)
			return
		}

		// Every Workload phase (see appsv1alpha1.Phase* et al.) gets an
		// explicit case here rather than falling through an `!=
		// PhaseRunning` catch-all — that used to send Stopped down the same
		// path as Deploying, showing an endlessly-refreshing "starting up"
		// spinner for a workload that was intentionally suspended and won't
		// start on its own.
		switch workload.Status.Phase {
		case appsv1alpha1.PhaseFailed:
			var message string
			if cond := apimeta.FindStatusCondition(workload.Status.Conditions, appsv1alpha1.ConditionTypeReady); cond != nil {
				message = cond.Message
			}
			renderFailedPage(w, r, name, message, gatewayPageRefreshSeconds)
			return
		case appsv1alpha1.PhaseStopped:
			renderStoppedPage(w, r, name)
			return
		case appsv1alpha1.PhaseRunning:
			// Fall through to Service resolution below.
		default:
			renderLoadingPage(w, r, name, gatewayPageRefreshSeconds)
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
		// ClusterIP is what makes this a real, dialable network target — a
		// headless Service (ClusterIP: None) has none, and reconcileService
		// (internal/controller/workload_controller.go) never sets one, so
		// this is a defensive check against a future change there, not a
		// case expected to trigger today.
		if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == corev1.ClusterIPNone {
			log.Error(fmt.Errorf("service has no usable ClusterIP: %q", svc.Spec.ClusterIP), "resolving target service")
			http.Error(w, "workload service has no usable ClusterIP", http.StatusBadGateway)
			return
		}
		var port int32
		var scheme string
		var found bool
		for _, sp := range svc.Spec.Ports {
			if sp.Name == entrypoint {
				port, found = sp.Port, true
				scheme = schemeHTTP
				if sp.Name == serviceProxyScheme {
					scheme = serviceProxyScheme
				}
				break
			}
		}
		if !found {
			http.Error(w, "unknown entrypoint", http.StatusNotFound)
			return
		}

		targetHost := fmt.Sprintf("%s:%d", svc.Spec.ClusterIP, port)

		proxy := &httputil.ReverseProxy{
			Transport: p.transport,
			Director: func(req *http.Request) {
				q := req.URL.Query()
				q.Del("token") // never forward our own gateway token upstream
				req.URL.Scheme = scheme
				req.URL.Host = targetHost
				req.URL.Path = "/" + subpath
				req.URL.RawQuery = q.Encode()
				req.Host = targetHost
			},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				log.Error(err, "proxying to workload service")
				http.Error(w, "upstream proxy error", http.StatusBadGateway)
			},
		}
		proxy.ServeHTTP(w, r)
	}
}

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

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	appsv1alpha1 "github.com/gojnimer-labs/ai-cloud-operator/api/v1alpha1"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/api"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/capacity"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/controller"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/convexclient"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/gateway"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/metrics"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/podexec"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/provisioning"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/tokenstore"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/workloadns"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(appsv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// WORKLOAD_NAMESPACE is read here (rather than inside
	// setupConvexIntegration, where every other Convex-related env var is
	// read) because it also has to seed the manager's Cache.DefaultNamespaces
	// below — every Workload CR, and the Deployment/Service the reconciler
	// creates for it, always lives in this one namespace (see
	// internal/provisioning.WorkloadCreator), so scoping the manager's cache
	// to it is what lets two operator instances (e.g. "prod" and "dev", each
	// with a distinct WORKLOAD_NAMESPACE) coexist in the same cluster without
	// reconciling each other's objects.
	workloadNamespace := os.Getenv("WORKLOAD_NAMESPACE")
	if workloadNamespace == "" {
		setupLog.Error(errors.New("WORKLOAD_NAMESPACE must be set"), "missing required env")
		os.Exit(1)
	}

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Client: client.Options{
			// internal/tokenstore reads/writes exactly one named Secret
			// under an RBAC grant scoped to get/update/create on that name
			// only — never list/watch. Left in the default cache, the
			// manager's shared informer tries to List/Watch all Secrets to
			// serve that Get, gets 403, and the resulting stuck reflector
			// stalls the whole shared cache (dragging down the Workload and
			// Deployment watches with it) until the hardcoded 2-minute
			// CacheSyncTimeout kills the manager. Excluding Secret routes
			// tokenstore straight to the API server instead.
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
		// Scopes every namespaced type this manager's cache serves (Workload,
		// Deployment, Service) to WORKLOAD_NAMESPACE, so two operator
		// instances in the same cluster never watch/reconcile each other's
		// objects. Cluster-scoped types (e.g. Node, read by
		// internal/capacity) are unaffected by DefaultNamespaces.
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				workloadNamespace: {},
			},
		},
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "75dfcc5e.aicloud.dev",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}
	setupLog.Info("workload cache scoped", "namespace", workloadNamespace)

	ctx := ctrl.SetupSignalHandler()

	convexRunnable, err := setupConvexIntegration(ctx, mgr, workloadNamespace)
	if err != nil {
		setupLog.Error(err, "Failed to set up convex registration/heartbeat and operator api")
		os.Exit(1)
	}

	if err := (&controller.WorkloadReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		ConvexClient: convexRunnable,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "workload")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// setupConvexIntegration wires the two runnables that implement this
// operator's side of the register/heartbeat/deploy contract with Convex:
//   - convexclient.Runnable registers with Convex once (reusing a token
//     persisted in a Secret across restarts when possible) and heartbeats
//     on a fixed interval thereafter, also watching ENROLLMENT_SECRET for
//     an out-of-band rotation.
//   - api.Server exposes the inbound HTTP API Convex calls to
//     deploy/delete/inspect Workload custom resources, authenticated with
//     the deploy token minted at registration.
//
// Both are added to the manager via mgr.Add so their lifecycle (start after
// the cache syncs, stop on SIGTERM) is managed the same way as the
// reconcile loop, with no bespoke goroutine plumbing in main. The returned
// *convexclient.Runnable is also wired into the WorkloadReconciler so it can
// report workload lifecycle events back to Convex.
func setupConvexIntegration(
	ctx context.Context, mgr ctrl.Manager, workloadNamespace string,
) (*convexclient.Runnable, error) {
	convexBaseURL := os.Getenv("CONVEX_BASE_URL")
	operatorName := os.Getenv("OPERATOR_NAME")
	operatorExternalURL := os.Getenv("OPERATOR_EXTERNAL_URL")
	podNamespace := os.Getenv("POD_NAMESPACE")

	missingRequiredEnv := convexBaseURL == "" || operatorName == "" ||
		operatorExternalURL == "" || podNamespace == ""
	if missingRequiredEnv {
		return nil, errors.New(
			"CONVEX_BASE_URL, OPERATOR_NAME, OPERATOR_EXTERNAL_URL, " +
				"and POD_NAMESPACE must all be set")
	}

	// ENROLLMENT_SECRET comes from a mounted volume, not an env var — see
	// convexclient.EnrollmentSecretPath — so the same watcher used for the
	// initial value below is reused for checkEnrollmentSecret's ongoing
	// rotation checks, rather than reading it two different ways.
	enrollment := convexclient.NewEnrollmentSecretWatcher("")
	enrollmentSecret, err := enrollment.Current(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading initial enrollment secret: %w", err)
	}
	if enrollmentSecret == "" {
		return nil, errors.New("enrollment secret file is empty")
	}

	// GATEWAY_SIGNING_SECRET is never shared with Convex or anyone else — it
	// only has to be internally consistent with itself across this
	// operator's own restarts — so rather than asking a human to mint and
	// hand it one, the operator generates it once and persists it in its own
	// namespace (see internal/gateway.KeyStore).
	gatewaySigningSecret, err := gateway.NewKeyStore(mgr.GetClient(), podNamespace).LoadOrGenerate(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading or generating gateway signing key: %w", err)
	}

	// WORKLOAD_NAMESPACE is the single, fixed namespace every workload this
	// operator manages gets deployed into — an install-time decision, not
	// something Convex chooses or sends per-request (see internal/api.Server
	// and internal/gateway.ServiceProxy). A blind create, so this is safe to
	// call before mgr.Start() (see workloadns.EnsureNamespace).
	if err := workloadns.EnsureNamespace(ctx, mgr.GetClient(), workloadNamespace); err != nil {
		return nil, fmt.Errorf("ensuring workload namespace: %w", err)
	}

	apiListenAddr := os.Getenv("API_LISTEN_ADDR")
	if apiListenAddr == "" {
		apiListenAddr = ":8443"
	}

	heartbeatInterval := 30 * time.Second
	if raw := os.Getenv("HEARTBEAT_INTERVAL"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, errors.New("invalid HEARTBEAT_INTERVAL: " + err.Error())
		}
		heartbeatInterval = parsed
	}

	// OPERATOR_VERSION is optional — display-only on Convex's fleet table
	// (see convexclient.Config.Version's doc comment). Left unset in the
	// kustomize path; the Helm chart sources it from .Chart.AppVersion, so
	// a Helm install reports its version automatically with no manual
	// wiring.
	operatorVersion := os.Getenv("OPERATOR_VERSION")

	// OPERATOR_TAGS is a comma-separated list, e.g. "gpu,on-prem". Presence
	// of the env var (LookupEnv), not truthiness of its value, decides
	// whether this operator reports tags at all — an unset var leaves
	// admin-set tags in Convex alone, while a set-but-empty value is a
	// deliberate "I have no tags" report that still locks them against
	// admin edits (see convexclient.Config.Tags's doc comment).
	var operatorTags []string
	if raw, ok := os.LookupEnv("OPERATOR_TAGS"); ok {
		operatorTags = []string{}
		if raw != "" {
			for tag := range strings.SplitSeq(raw, ",") {
				if trimmed := strings.TrimSpace(tag); trimmed != "" {
					operatorTags = append(operatorTags, trimmed)
				}
			}
		}
	}

	// One WorkloadCreator + one WorkloadDestroyer, shared between the
	// manual /workloads* HTTP path (api.New, still reachable for
	// local/manual testing) and the claim-consumption loop
	// (convexclient.NewRunnable) that's the normal flow once Convex owns
	// create/destroy/redeploy as claimable requests — see
	// internal/provisioning.
	workloadCreator := provisioning.NewWorkloadCreator(mgr.GetClient(), workloadNamespace)
	workloadDestroyer := provisioning.NewWorkloadDestroyer(mgr.GetClient(), workloadNamespace)

	// capacityTracker gates processClaimable against local headroom before
	// ever calling ClaimWorkload — see internal/capacity's package doc for
	// why this is a purely operator-side decision, never reported to Convex
	// for gating. Registered via mgr.Add for lifecycle parity with the other
	// long-running components below, though its actual computation is
	// pull-based from heartbeatOnce, not driven by Start.
	capacityTracker := capacity.NewTracker(mgr.GetClient(), workloadNamespace)
	if err := mgr.Add(capacityTracker); err != nil {
		return nil, err
	}

	// metricsCollector is built here, ahead of convexRunnable, so the same
	// instance can be wired into RunnableConfig.Usage below (feeding
	// heartbeatOnce's live cluster/managed usage figures every 30s) as well
	// as metricsReporter further down (its own, much coarser 5-minute
	// network-bytes report) — one clientset, no duplicate construction.
	metricsCollector, err := metrics.NewCollector(mgr.GetClient(), mgr.GetConfig(), workloadNamespace)
	if err != nil {
		return nil, fmt.Errorf("building metrics collector: %w", err)
	}

	convexRunnable := convexclient.NewRunnable(convexclient.RunnableConfig{
		Client: convexclient.New(convexclient.Config{
			BaseURL:          convexBaseURL,
			EnrollmentSecret: enrollmentSecret,
			OperatorName:     operatorName,
			ExternalURL:      operatorExternalURL,
			Version:          operatorVersion,
			Tags:             operatorTags,
		}),
		Store:             tokenstore.New(mgr.GetClient(), podNamespace),
		Enrollment:        enrollment,
		HeartbeatInterval: heartbeatInterval,
		Creator:           workloadCreator,
		Destroyer:         workloadDestroyer,
		Capacity:          capacityTracker,
		Usage:             metricsCollector,
	})
	if err := mgr.Add(convexRunnable); err != nil {
		return nil, err
	}

	// metricsReportInterval is deliberately its own, much coarser knob than
	// HEARTBEAT_INTERVAL — see internal/metrics.Reporter's doc comment for
	// why usage reporting is a segregated responsibility from
	// heartbeat/claim-discovery, not just a segregated HTTP route.
	metricsReportInterval := 5 * time.Minute
	if raw := os.Getenv("METRICS_REPORT_INTERVAL"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, errors.New("invalid METRICS_REPORT_INTERVAL: " + err.Error())
		}
		metricsReportInterval = parsed
	}
	metricsReporter := metrics.NewReporter(metricsCollector, convexRunnable, metricsReportInterval)
	if err := mgr.Add(metricsReporter); err != nil {
		return nil, err
	}

	proxy := gateway.NewServiceProxy(mgr.GetClient(), workloadNamespace)

	podExecutor, err := podexec.New(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("building pod executor: %w", err)
	}

	apiServer := api.New(api.Config{
		Client:          mgr.GetClient(),
		ListenAddr:      apiListenAddr,
		Token:           convexRunnable.CurrentDeployToken,
		GatewaySecret:   gatewaySigningSecret,
		GatewayVerifier: convexRunnable,
		Proxy:           proxy,
		PodExecutor:     podExecutor,
		Creator:         workloadCreator,
		Destroyer:       workloadDestroyer,
		Namespace:       workloadNamespace,
	})
	if err := mgr.Add(apiServer); err != nil {
		return nil, err
	}

	return convexRunnable, nil
}

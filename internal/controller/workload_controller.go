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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/gojnimer-labs/ai-cloud-operator/api/v1alpha1"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/catalog"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/convexclient"
)

// WorkloadNotifier lets the reconciler tell Convex about workload lifecycle
// events so its ownership table stays in sync with the cluster automatically
// — including workloads created/deleted directly with kubectl, bypassing
// Convex entirely. Optional: nil disables the callback (e.g. in tests, or a
// future standalone-operator mode with no Convex attached).
type WorkloadNotifier interface {
	UpsertWorkload(ctx context.Context, info convexclient.WorkloadInfo) error
	RemoveWorkload(ctx context.Context, name, namespace string) error
}

// WorkloadReconciler reconciles a Workload object.
//
// It derives a Deployment + Service from the Workload spec (owner-referenced
// for garbage collection) and writes their observed state back onto
// Workload.Status. Secret/PVC/HTTPRoute/Middleware/TunnelBinding reconciliation
// is deliberately out of scope for this POC.
type WorkloadReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	ConvexClient WorkloadNotifier
}

const (
	labelName      = "app.kubernetes.io/name"
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelInstance  = "app.kubernetes.io/instance"
	labelUserID    = "apps.aicloud.dev/user-id"
	managedByValue = "ai-cloud-operator"

	conditionTypeReady = "Ready"

	phaseDeploying = "Deploying"
	phaseRunning   = "Running"
	phaseFailed    = "Failed"

	defaultReplicas      = int32(1)
	defaultContainerPort = int32(8080)

	statusRequeueInterval = 10 * time.Second
)

// +kubebuilder:rbac:groups=apps.aicloud.dev,resources=workloads,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.aicloud.dev,resources=workloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.aicloud.dev,resources=workloads/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *WorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var workload appsv1alpha1.Workload
	if err := r.Get(ctx, req.NamespacedName, &workload); err != nil {
		if apierrors.IsNotFound(err) {
			r.notifyRemoved(ctx, log, req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting Workload: %w", err)
	}

	// Captured before render/status update overwrite ObservedGeneration below
	// — true only on the reconcile immediately following a spec create/change,
	// so the Convex upsert callback fires once per spec version rather than on
	// every periodic status-polling pass.
	specChanged := workload.Status.ObservedGeneration != workload.Generation

	// selectorLabels stays exactly what it's always been — Deployment/Service
	// selectors are immutable after creation, so this set must never change
	// for a given workload. objectLabels is selectorLabels plus a UserID
	// label applied to object metadata only (never the selector), so adding
	// it can't ever collide with selector-immutability rules.
	selectorLabels := map[string]string{
		labelName:      workload.Name,
		labelManagedBy: managedByValue,
		labelInstance:  string(workload.UID),
	}
	objectLabels := make(map[string]string, len(selectorLabels)+1)
	for k, v := range selectorLabels {
		objectLabels[k] = v
	}
	if v, ok := sanitizeLabelValue(workload.Spec.UserID); ok {
		objectLabels[labelUserID] = v
	}

	rendered, err := r.render(&workload)
	if err != nil {
		return r.setFailed(ctx, &workload, fmt.Errorf("rendering workload: %w", err))
	}

	if err := r.reconcileDeployment(ctx, &workload, selectorLabels, objectLabels, rendered); err != nil {
		return r.setFailed(ctx, &workload, fmt.Errorf("reconciling deployment: %w", err))
	}

	if err := r.reconcileService(ctx, &workload, selectorLabels, objectLabels, rendered); err != nil {
		return r.setFailed(ctx, &workload, fmt.Errorf("reconciling service: %w", err))
	}

	if specChanged {
		r.notifyUpserted(ctx, log, &workload)
	}

	var deployment appsv1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &deployment); err != nil {
		return ctrl.Result{}, fmt.Errorf("getting deployment for status: %w", err)
	}

	desiredReplicas := defaultReplicas
	if workload.Spec.Replicas != nil {
		desiredReplicas = *workload.Spec.Replicas
	}

	workload.Status.ReadyReplicas = deployment.Status.ReadyReplicas
	workload.Status.ObservedGeneration = workload.Generation

	readyCondition := metav1.Condition{
		Type:               conditionTypeReady,
		ObservedGeneration: workload.Generation,
	}

	if desiredReplicas > 0 && deployment.Status.ReadyReplicas >= desiredReplicas {
		workload.Status.Phase = phaseRunning
		readyCondition.Status = metav1.ConditionTrue
		readyCondition.Reason = "DeploymentReady"
		readyCondition.Message = "backing Deployment has reached the desired ready replica count"
	} else {
		workload.Status.Phase = phaseDeploying
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = "DeploymentProgressing"
		readyCondition.Message = fmt.Sprintf("%d/%d replicas ready", deployment.Status.ReadyReplicas, desiredReplicas)
	}

	apimeta.SetStatusCondition(&workload.Status.Conditions, readyCondition)

	if err := r.Status().Update(ctx, &workload); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating workload status: %w", err)
	}

	if workload.Status.Phase != phaseRunning {
		log.Info("workload not yet running, requeueing", "phase", workload.Status.Phase)
		return ctrl.Result{RequeueAfter: statusRequeueInterval}, nil
	}

	return ctrl.Result{}, nil
}

// notifyUpserted tells Convex this workload's ownership info is current.
// Best-effort: a failure here is logged but never fails reconciliation —
// the Deployment/Service must not depend on Convex being reachable, and the
// next spec-changing reconcile (or a future retry policy) can catch up.
func (r *WorkloadReconciler) notifyUpserted(ctx context.Context, log logr.Logger, workload *appsv1alpha1.Workload) {
	if r.ConvexClient == nil {
		return
	}
	err := r.ConvexClient.UpsertWorkload(ctx, convexclient.WorkloadInfo{
		Name:         workload.Name,
		Namespace:    workload.Namespace,
		Subdomain:    workload.Spec.Subdomain,
		TemplateName: workload.Spec.TemplateName,
		UserID:       workload.Spec.UserID,
	})
	if err != nil {
		log.Error(err, "failed to notify convex of workload upsert (best-effort)")
	}
}

// notifyRemoved tells Convex this workload no longer exists. Best-effort,
// same reasoning as notifyUpserted.
func (r *WorkloadReconciler) notifyRemoved(ctx context.Context, log logr.Logger, key types.NamespacedName) {
	if r.ConvexClient == nil {
		return
	}
	if err := r.ConvexClient.RemoveWorkload(ctx, key.Name, key.Namespace); err != nil {
		log.Error(err, "failed to notify convex of workload removal (best-effort)")
	}
}

// setFailed marks the Workload as Failed and surfaces the reconcile error via
// a status condition, then returns it so the manager still applies its normal
// exponential-backoff requeue.
func (r *WorkloadReconciler) setFailed(ctx context.Context, workload *appsv1alpha1.Workload, reconcileErr error) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Error(reconcileErr, "reconcile failed")

	workload.Status.Phase = phaseFailed
	workload.Status.ObservedGeneration = workload.Generation
	apimeta.SetStatusCondition(&workload.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             "ReconcileError",
		Message:            reconcileErr.Error(),
		ObservedGeneration: workload.Generation,
	})
	if err := r.Status().Update(ctx, workload); err != nil {
		log.Error(err, "failed to update workload status after reconcile error")
	}

	return ctrl.Result{}, reconcileErr
}

// render produces the containers/volumes/service-ports for workload, either
// by dispatching to a catalog template (when Spec.TemplateName is set) or by
// falling back to the original raw image/containerPort/env fields.
func (r *WorkloadReconciler) render(workload *appsv1alpha1.Workload) (catalog.Rendered, error) {
	if workload.Spec.TemplateName == "" {
		if workload.Spec.Image == "" {
			return catalog.Rendered{}, fmt.Errorf("spec.image is required when spec.templateName is unset")
		}
		return legacyRendered(workload), nil
	}

	tmpl, ok := catalog.Get(workload.Spec.TemplateName)
	if !ok {
		return catalog.Rendered{}, fmt.Errorf("unknown template %q", workload.Spec.TemplateName)
	}

	rawParams, err := configToParams(workload.Spec.Config)
	if err != nil {
		return catalog.Rendered{}, fmt.Errorf("parsing config: %w", err)
	}

	resolvedParams, err := catalog.ResolveParams(tmpl, rawParams)
	if err != nil {
		return catalog.Rendered{}, fmt.Errorf("resolving template parameters: %w", err)
	}

	return tmpl.Build(resolvedParams)
}

// legacyRendered is the original raw-image deploy path, unchanged in
// behavior from before templates existed.
func legacyRendered(workload *appsv1alpha1.Workload) catalog.Rendered {
	containerPort := workload.Spec.ContainerPort
	if containerPort == 0 {
		containerPort = defaultContainerPort
	}

	return catalog.Rendered{
		Containers: []corev1.Container{
			{
				Name:  "workload",
				Image: workload.Spec.Image,
				Ports: []corev1.ContainerPort{
					{ContainerPort: containerPort},
				},
				Env: workload.Spec.Env,
			},
		},
		ServicePorts: []corev1.ServicePort{
			{
				Name:       "http",
				Port:       containerPort,
				TargetPort: intstr.FromInt32(containerPort),
			},
		},
	}
}

// configToParams unmarshals the CRD's loose Config bag into the map catalog
// templates expect. A nil Config (no parameters supplied) is not an error —
// it just means every template parameter falls back to its declared default.
func configToParams(config *apiextensionsv1.JSON) (map[string]any, error) {
	if config == nil || len(config.Raw) == 0 {
		return map[string]any{}, nil
	}
	var params map[string]any
	if err := json.Unmarshal(config.Raw, &params); err != nil {
		return nil, err
	}
	return params, nil
}

func (r *WorkloadReconciler) reconcileDeployment(ctx context.Context, workload *appsv1alpha1.Workload, selectorLabels, objectLabels map[string]string, rendered catalog.Rendered) error {
	replicas := defaultReplicas
	if workload.Spec.Replicas != nil {
		replicas = *workload.Spec.Replicas
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workload.Name,
			Namespace: workload.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		deployment.Labels = objectLabels
		deployment.Spec.Replicas = &replicas
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorLabels}
		deployment.Spec.Template.ObjectMeta.Labels = objectLabels
		deployment.Spec.Template.Spec.Containers = rendered.Containers
		deployment.Spec.Template.Spec.InitContainers = rendered.InitContainers
		deployment.Spec.Template.Spec.Volumes = rendered.Volumes
		return controllerutil.SetControllerReference(workload, deployment, r.Scheme)
	})
	return err
}

func (r *WorkloadReconciler) reconcileService(ctx context.Context, workload *appsv1alpha1.Workload, selectorLabels, objectLabels map[string]string, rendered catalog.Rendered) error {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workload.Name,
			Namespace: workload.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		service.Labels = objectLabels
		service.Spec.Selector = selectorLabels
		service.Spec.Ports = rendered.ServicePorts
		return controllerutil.SetControllerReference(workload, service, r.Scheme)
	})
	return err
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Workload{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Named("workload").
		Complete(r)
}

var labelValueRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

const maxLabelValueLen = 63

// sanitizeLabelValue returns (value, true) if raw is already a valid k8s
// label value. Otherwise returns ("", false) — the caller should skip
// setting the label rather than fail reconciliation, since UserID is
// informational/future-proofing only, not load-bearing for reconcile
// correctness.
func sanitizeLabelValue(raw string) (string, bool) {
	if raw == "" || len(raw) > maxLabelValueLen || !labelValueRe.MatchString(raw) {
		return "", false
	}
	return raw, true
}

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
	"errors"
	"fmt"
	"maps"
	"regexp"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/gojnimer-labs/ai-cloud-operator/api/v1alpha1"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/catalog"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/convexclient"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/labels"
)

// WorkloadNotifier lets the reconciler tell Convex about workload lifecycle
// events so its ownership table stays in sync with the cluster automatically
// — including workloads created/deleted directly with kubectl, bypassing
// Convex entirely. Optional: nil disables the callback (e.g. in tests, or a
// future standalone-operator mode with no Convex attached).
type WorkloadNotifier interface {
	UpsertWorkload(ctx context.Context, info convexclient.WorkloadInfo) error
	RemoveWorkload(ctx context.Context, name, namespace string) error
	// ReportLifecycle tells Convex a claimed create/redeploy/stop/resume
	// attempt reached a terminal-for-now state ("active", "stopped", or
	// "failed", with reason set for the latter). workloadID (the
	// apps.aicloud.dev/workload-id label's value, when present) is passed
	// alongside name so Convex can still resolve the row even before this
	// Workload's first successful upsert has recorded its name — see
	// setFailed and syncConvexLifecyclePhase. retryable marks a "failed"
	// report as transient/self-diagnosed rather than a genuine, terminal
	// reconcile error: Convex releases the claim back to any tag-matching
	// operator immediately instead of waiting out a lease (see
	// checkUnschedulable, the one caller that ever passes true — setFailed's
	// reconcile-error reports always pass false, since a bad spec/unknown
	// template isn't something a different operator could succeed at
	// either).
	ReportLifecycle(ctx context.Context, name, workloadID, phase, reason string, retryable bool) error
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
	labelName     = "app.kubernetes.io/name"
	labelInstance = "app.kubernetes.io/instance"
	labelUserID   = "apps.aicloud.dev/user-id"

	conditionTypeConvexSynced          = "ConvexSynced"
	conditionTypeConvexLifecycleSynced = "ConvexLifecycleSynced"

	// Condition Reason values below are the stable, machine-readable half of
	// every status condition this controller sets — Message stays free-form
	// English (debug detail for kubectl/logs), but Reason is what a
	// consumer (ai-cloud-v2's Convex backend, then its frontend) keys a
	// translated string off, the same way ai-cloud-operator's own
	// catalog function results already use a "stable, namespaced message
	// key" (see docs/catalog-parameters.md). Kept as PascalCase identifiers
	// rather than the dot-namespaced form used elsewhere in this repo:
	// metav1.Condition.Reason is validated by the apiserver against
	// `^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`, which rejects dots.
	// Centralizing these here doesn't change any of their wire values —
	// every string below already existed inline at each call site.
	reasonWorkloadSuspended     = "WorkloadSuspended"
	reasonDeploymentReady       = "DeploymentReady"
	reasonDeploymentProgressing = "DeploymentProgressing"
	reasonNotifyFailed          = "NotifyFailed"
	reasonNotified              = "Notified"
	reasonAlreadyResolved       = "AlreadyResolved"
	reasonReconcileError        = "ReconcileError"
	// reasonInvalidSpec/reasonUnknownTemplate split out of the generic
	// ReconcileError bucket: these two are the common, user-caused
	// misconfigurations from render() (bad/missing spec.image, unknown
	// spec.templateName) — worth a distinct translated message on the
	// frontend instead of a generic "reconcile failed." Every other
	// reconcile failure (deployment/service apply, status update, template
	// param resolution) stays under ReconcileError — see setFailed's own
	// doc comment for why those aren't split out too.
	reasonInvalidSpec     = "InvalidSpec"
	reasonUnknownTemplate = "UnknownTemplate"

	// lifecyclePhaseActive/lifecyclePhaseFailed/lifecyclePhaseStopped are the
	// phase values sent to WorkloadNotifier.ReportLifecycle — see
	// convex/workloads/mutations.ts's reportLifecycle in ai-cloud-v2 for the
	// Convex side that consumes them.
	lifecyclePhaseActive  = "active"
	lifecyclePhaseFailed  = "failed"
	lifecyclePhaseStopped = "stopped"

	defaultContainerPort = int32(8080)

	statusRequeueInterval = 10 * time.Second

	// convexNotifyTimeout bounds how long a single Convex notify call inside
	// Reconcile can hold up the reconciler — shorter than the shared HTTP
	// client's 10s timeout (internal/convexclient.Client) on purpose, since
	// this one runs inline in the hot path rather than in a background
	// heartbeat loop.
	convexNotifyTimeout = 3 * time.Second

	// maxConcurrentReconciles bumped from controller-runtime's default of 1
	// so a slow/unreachable Convex during syncConvex doesn't serialize
	// reconciliation of every other Workload in the cluster behind it.
	maxConcurrentReconciles = 5

	// workloadFinalizer holds deletion of a Workload open until Convex has
	// confirmed the removal notification (see notifyRemoved) — without it,
	// a failed notify has nothing left to retry against once the CR is gone
	// from etcd. Retried indefinitely on notify failure, the same tradeoff
	// syncConvex/syncConvexLifecyclePhase already accept for other
	// best-effort Convex syncs. If Convex is unreachable for a prolonged
	// outage and a Workload needs to be force-deleted anyway, clear it
	// manually: `kubectl patch workload <name> --type=merge -p
	// '{"metadata":{"finalizers":[]}}'`.
	workloadFinalizer = "apps.aicloud.dev/workload-finalizer"

	// unschedulableGracePeriod bounds how long a Pod may sit with
	// PodScheduled=False/Reason=Unschedulable before checkUnschedulable
	// gives up on ever placing it on this cluster and releases the claim
	// for another operator to try — long enough that a normal scheduling
	// blip (a burst of other pods landing first, a node briefly cordoned)
	// never triggers it, short enough to be meaningfully faster than
	// Convex's own ~60min blind lease exhaustion (see releaseClaim in
	// ai-cloud-v2's convex/workloads/mutations.ts).
	unschedulableGracePeriod = 3 * time.Minute
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
			// Fallback for an object that somehow left etcd without going
			// through reconcileDelete below (e.g. the documented manual
			// finalizer-clear escape hatch) — best-effort, same reasoning
			// as notifyRemoved itself.
			_ = r.notifyRemoved(ctx, req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting Workload: %w", err)
	}

	if !workload.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &workload)
	}

	if !controllerutil.ContainsFinalizer(&workload, workloadFinalizer) {
		controllerutil.AddFinalizer(&workload, workloadFinalizer)
		if err := r.Update(ctx, &workload); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// No early return: r.Update above refreshes workload's
		// ResourceVersion/UID in place, so the rest of this reconcile (and
		// its own r.Status().Update at the end) proceeds against the
		// now-current object in the same pass, exactly as it would have
		// without a finalizer — one Reconcile call still fully processes a
		// newly-created Workload.
	}

	// selectorLabels stays exactly what it's always been — Deployment/Service
	// selectors are immutable after creation, so this set must never change
	// for a given workload. objectLabels is selectorLabels plus a UserID
	// label applied to object metadata only (never the selector), so adding
	// it can't ever collide with selector-immutability rules.
	selectorLabels := map[string]string{
		labelName:        workload.Name,
		labels.ManagedBy: labels.ManagedByValue,
		labelInstance:    string(workload.UID),
	}
	objectLabels := make(map[string]string, len(selectorLabels)+1)
	maps.Copy(objectLabels, selectorLabels)
	if v, ok := sanitizeLabelValue(workload.Spec.UserID); ok {
		objectLabels[labelUserID] = v
	}

	rendered, err := r.render(&workload)
	if err != nil {
		reason := reasonReconcileError
		switch {
		case errors.Is(err, errImageRequired):
			reason = reasonInvalidSpec
		case errors.Is(err, errUnknownTemplate):
			reason = reasonUnknownTemplate
		}
		return r.setFailedWithReason(ctx, &workload, reason, fmt.Errorf("rendering workload: %w", err))
	}

	// A conflict from CreateOrUpdate's read-modify-write (409, "the object
	// has been modified; please apply your changes to the latest version and
	// try again") is routine, not a real failure — e.g. another concurrent
	// reconcile of this same Workload, or the Deployment/Service being
	// deleted/recreated out-of-band (a human debugging via kubectl/Headlamp)
	// between our own Get and Update. Returning it bare lets
	// controller-runtime's normal requeue-with-backoff retry immediately,
	// almost always succeeding on the very next attempt — critically,
	// WITHOUT going through setFailed, which would report a non-retryable
	// "failed" to Convex for something that was never actually broken. That
	// report is also one-way: reportLifecycle only accepts a phase report
	// against an in-flight status, so once Convex marks a row "failed" for
	// this, a later genuine "active" report (once the Deployment really
	// does become ready) is rejected as stale and the row is stuck at
	// "failed" forever, even though nothing was ever wrong.
	if err := r.reconcileDeployment(ctx, &workload, selectorLabels, objectLabels, rendered); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{}, err
		}
		return r.setFailed(ctx, &workload, fmt.Errorf("reconciling deployment: %w", err))
	}

	if err := r.reconcileService(ctx, &workload, selectorLabels, objectLabels, rendered); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{}, err
		}
		return r.setFailed(ctx, &workload, fmt.Errorf("reconciling service: %w", err))
	}

	if r.checkUnschedulable(ctx, &workload, selectorLabels) {
		// Claim released and this Workload deleted — nothing left to do.
		return ctrl.Result{}, nil
	}

	convexSynced := r.syncConvex(ctx, &workload)

	var deployment appsv1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &deployment); err != nil {
		return ctrl.Result{}, fmt.Errorf("getting deployment for status: %w", err)
	}

	lifecycleSynced := r.updateStatus(ctx, &workload, &deployment)

	if err := r.Status().Update(ctx, &workload); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating workload status: %w", err)
	}

	settled := workload.Status.Phase == appsv1alpha1.PhaseRunning || workload.Status.Phase == appsv1alpha1.PhaseStopped
	if !settled || !convexSynced || !lifecycleSynced {
		log.Info("workload not yet ready to stop requeueing", "phase", workload.Status.Phase, "convexSynced", convexSynced, "lifecycleSynced", lifecycleSynced)
		return ctrl.Result{RequeueAfter: statusRequeueInterval}, nil
	}

	return ctrl.Result{}, nil
}

// checkUnschedulable looks for a Pod belonging to workload that's been stuck
// failing to schedule (PodScheduled=False, Reason=Unschedulable) for longer
// than unschedulableGracePeriod — a strong signal this cluster can never fit
// it (e.g. a template requesting more CPU than any node here will ever have
// free), which this reconciler's own indefinite retry-forever loop
// (statusRequeueInterval, no timeout of its own) has no way to detect on its
// own. Only checked once past Suspended/already-Running — a suspended
// workload has no desired pods, and an already-Running one already proved
// it schedules fine here (a later eviction is out of scope for this check).
//
// On a genuine, sustained detection: reports a retryable failure to Convex
// (see WorkloadNotifier.ReportLifecycle's retryable doc — releases the
// claim to any other tag-matching operator immediately, rather than waiting
// out Convex's own ~60min blind lease exhaustion) and, once that lands,
// deletes this Workload so this cluster stops retrying it forever and its
// Deployment/Service are freed (garbage-collected via owner references).
// The report must land before the delete: reportDestroyed (fired by the
// delete, via notifyRemoved) looks the row up by (operatorId, name) — since
// the retryable report already cleared both on Convex's row, that lookup
// safely no-ops instead of incorrectly patching a reopened row to
// "destroyed".
//
// Best-effort like every other Convex notify in this file: a failed report
// or delete just means the same stuck Pod condition is still there on the
// next reconcile, naturally retrying this whole sequence — no separate
// retry bookkeeping needed. Returns whether the claim was released (and
// this Workload deleted), telling Reconcile there's nothing further to do
// this pass.
func (r *WorkloadReconciler) checkUnschedulable(ctx context.Context, workload *appsv1alpha1.Workload, selectorLabels map[string]string) bool {
	if workload.Spec.Suspended || desiredReplicaCount(workload) == 0 || workload.Status.Phase == appsv1alpha1.PhaseRunning {
		return false
	}

	log := logf.FromContext(ctx)

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(workload.Namespace), client.MatchingLabels(selectorLabels)); err != nil {
		log.Error(err, "listing pods to check scheduling status")
		return false
	}

	var stuckMessage string
	for _, pod := range pods.Items {
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodScheduled &&
				cond.Status == corev1.ConditionFalse &&
				cond.Reason == corev1.PodReasonUnschedulable &&
				time.Since(cond.LastTransitionTime.Time) >= unschedulableGracePeriod {
				stuckMessage = cond.Message
			}
		}
	}
	if stuckMessage == "" {
		return false
	}

	reason := fmt.Sprintf("insufficient cluster capacity: %s", stuckMessage)
	if err := r.notifyLifecycle(ctx, workload, lifecyclePhaseFailed, reason, true); err != nil {
		return false
	}
	if err := r.Delete(ctx, workload); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "deleting workload after releasing unschedulable claim")
		return false
	}
	log.Info("released claim: workload will never schedule on this cluster", "reason", reason)
	return true
}

// updateStatus computes workload's Phase and Ready condition from
// deployment's currently observed state, and — when the phase just settled
// into Running or Stopped — reports that lifecycle transition to Convex via
// syncConvexLifecyclePhase. Returns whether Convex is now up to date for
// this generation (mirrors syncConvex's own return value; see Reconcile for
// how both feed into the settled/requeue decision).
func (r *WorkloadReconciler) updateStatus(ctx context.Context, workload *appsv1alpha1.Workload, deployment *appsv1.Deployment) bool {
	desiredReplicas := desiredReplicaCount(workload)

	workload.Status.ReadyReplicas = deployment.Status.ReadyReplicas
	workload.Status.ObservedGeneration = workload.Generation

	readyCondition := metav1.Condition{
		Type:               appsv1alpha1.ConditionTypeReady,
		ObservedGeneration: workload.Generation,
	}

	// lifecycleSynced defaults to true (nothing to report yet) and is only
	// actually attempted once the Deployment settles into Running or
	// Stopped — there is nothing to report before then. See
	// syncConvexLifecyclePhase.
	lifecycleSynced := true

	switch {
	case workload.Spec.Suspended && deployment.Status.ReadyReplicas == 0:
		workload.Status.Phase = appsv1alpha1.PhaseStopped
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = reasonWorkloadSuspended
		readyCondition.Message = "workload is intentionally stopped (spec.suspended=true)"
		lifecycleSynced = r.syncConvexLifecyclePhase(ctx, workload, lifecyclePhaseStopped)
	case desiredReplicas > 0 && deployment.Status.ReadyReplicas >= desiredReplicas:
		workload.Status.Phase = appsv1alpha1.PhaseRunning
		readyCondition.Status = metav1.ConditionTrue
		readyCondition.Reason = reasonDeploymentReady
		readyCondition.Message = "backing Deployment has reached the desired ready replica count"
		lifecycleSynced = r.syncConvexLifecyclePhase(ctx, workload, lifecyclePhaseActive)
	default:
		workload.Status.Phase = appsv1alpha1.PhaseDeploying
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = reasonDeploymentProgressing
		readyCondition.Message = fmt.Sprintf("%d/%d replicas ready", deployment.Status.ReadyReplicas, desiredReplicas)
	}

	apimeta.SetStatusCondition(&workload.Status.Conditions, readyCondition)
	return lifecycleSynced
}

// syncConvex tells Convex about workload's current ownership info when
// needed, and reports whether Convex is now up to date for this generation.
// "Needed" is: no successful attempt yet recorded for the current
// generation — tracked via the ConvexSynced condition's own
// ObservedGeneration, the same idiom already used for Ready, rather than a
// separate bespoke status field.
//
// Best-effort in the sense that a failure here must never fail
// Deployment/Service reconciliation or gate the Ready condition — but unlike
// a fire-and-forget log line, the caller retries a failed attempt on every
// subsequent reconcile (via the RequeueAfter this method's false return
// triggers in Reconcile) until it succeeds, so a Convex outage delays
// delivery of the ownership update rather than silently dropping it.
func (r *WorkloadReconciler) syncConvex(ctx context.Context, workload *appsv1alpha1.Workload) bool {
	if existing := apimeta.FindStatusCondition(workload.Status.Conditions, conditionTypeConvexSynced); existing != nil &&
		existing.Status == metav1.ConditionTrue && existing.ObservedGeneration == workload.Generation {
		return true
	}

	condition := metav1.Condition{
		Type:               conditionTypeConvexSynced,
		ObservedGeneration: workload.Generation,
	}
	if err := r.notifyUpserted(ctx, workload); err != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = reasonNotifyFailed
		condition.Message = err.Error()
	} else {
		condition.Status = metav1.ConditionTrue
		condition.Reason = reasonNotified
		condition.Message = "Convex has this workload's current ownership info"
	}
	apimeta.SetStatusCondition(&workload.Status.Conditions, condition)
	return condition.Status == metav1.ConditionTrue
}

// notifyUpserted tells Convex this workload's ownership info is current.
// See syncConvex for how a failure here gets retried.
func (r *WorkloadReconciler) notifyUpserted(ctx context.Context, workload *appsv1alpha1.Workload) error {
	if r.ConvexClient == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, convexNotifyTimeout)
	defer cancel()
	err := r.ConvexClient.UpsertWorkload(ctx, convexclient.WorkloadInfo{
		Name:         workload.Name,
		Namespace:    workload.Namespace,
		Subdomain:    workload.Spec.Subdomain,
		TemplateName: workload.Spec.TemplateName,
		UserID:       workload.Spec.UserID,
		WorkloadID:   workload.Labels[labels.WorkloadID],
	})
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to notify convex of workload upsert")
	}
	return err
}

// syncConvexLifecyclePhase tells Convex this workload reached phase
// ("active" or "stopped") for the current generation, and reports whether
// Convex is now up to date — same idiom as syncConvex/ConvexSynced
// (condition's own ObservedGeneration tracks "already reported for this
// generation," rather than a bespoke status field), keyed by Generation so a
// Suspended flip (which bumps Generation the same as any other spec change)
// naturally triggers a fresh sync. Kept as its own condition/call site,
// separate from setFailed, because setFailed returns immediately and never
// reaches this point — "active"/"stopped" and "failed" are reported from
// genuinely different places in Reconcile, not a shared helper.
//
// Best-effort in the same sense as syncConvex: a failure here must never
// gate the Ready condition, but the caller retries on every subsequent
// reconcile (via the RequeueAfter this method's false return triggers)
// until it succeeds.
func (r *WorkloadReconciler) syncConvexLifecyclePhase(ctx context.Context, workload *appsv1alpha1.Workload, phase string) bool {
	if existing := apimeta.FindStatusCondition(workload.Status.Conditions, conditionTypeConvexLifecycleSynced); existing != nil &&
		existing.Status == metav1.ConditionTrue && existing.ObservedGeneration == workload.Generation {
		return true
	}

	condition := metav1.Condition{
		Type:               conditionTypeConvexLifecycleSynced,
		ObservedGeneration: workload.Generation,
	}
	switch err := r.notifyLifecycle(ctx, workload, phase, "", false); {
	case err == nil:
		condition.Status = metav1.ConditionTrue
		condition.Reason = reasonNotified
		condition.Message = fmt.Sprintf("Convex has been told this workload is %s", phase)
	case errors.Is(err, convexclient.ErrLifecycleStale):
		// Convex's row already reflects a resolved active/stopped state for
		// this workload from an earlier report (e.g. a setFailed call during
		// this same generation that ai-cloud-v2's resolveLifecycleStatus
		// reinterpreted as "active" rather than a hard failure — see
		// convex/workloads/mutations.ts#reportLifecycle). There's nothing
		// left to tell Convex for this generation, and retrying this exact
		// call can never succeed until a fresh redeploy/stop/resume moves
		// the row back into an in-flight status — that call reports
		// lifecycle again on its own once it lands. Marking this synced
		// instead of retrying every statusRequeueInterval avoids an
		// unwinnable retry loop that would otherwise run forever.
		condition.Status = metav1.ConditionTrue
		condition.Reason = reasonAlreadyResolved
		condition.Message = "Convex already reflects this workload's current phase from an earlier report"
	default:
		condition.Status = metav1.ConditionFalse
		condition.Reason = reasonNotifyFailed
		condition.Message = err.Error()
	}
	apimeta.SetStatusCondition(&workload.Status.Conditions, condition)
	return condition.Status == metav1.ConditionTrue
}

// notifyLifecycle reports a create/redeploy attempt's terminal-for-now
// phase to Convex, passing both this Workload's real name and (when
// present) its apps.aicloud.dev/workload-id label — see
// WorkloadNotifier.ReportLifecycle for why both are sent, and for why
// retryable is almost always false here.
func (r *WorkloadReconciler) notifyLifecycle(ctx context.Context, workload *appsv1alpha1.Workload, phase, reason string, retryable bool) error {
	if r.ConvexClient == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, convexNotifyTimeout)
	defer cancel()
	err := r.ConvexClient.ReportLifecycle(ctx, workload.Name, workload.Labels[labels.WorkloadID], phase, reason, retryable)
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to notify convex of workload lifecycle", "phase", phase)
	}
	return err
}

// notifyRemoved tells Convex this workload no longer exists. Retried by
// reconcileDelete (via workloadFinalizer) until it succeeds; the one
// remaining fire-and-forget caller is Reconcile's IsNotFound fallback, for
// an object that left etcd some other way.
func (r *WorkloadReconciler) notifyRemoved(ctx context.Context, key types.NamespacedName) error {
	if r.ConvexClient == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, convexNotifyTimeout)
	defer cancel()
	err := r.ConvexClient.RemoveWorkload(ctx, key.Name, key.Namespace)
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to notify convex of workload removal")
	}
	return err
}

// reconcileDelete handles a Workload with a non-zero DeletionTimestamp:
// retries notifyRemoved until it succeeds, then releases workloadFinalizer
// so the object actually disappears. Kept separate from setFailed since a
// deleting Workload can't take a Status().Update the way a live one can —
// the finalizer itself is the only state left to track a pending retry
// against once deletion has started.
func (r *WorkloadReconciler) reconcileDelete(ctx context.Context, workload *appsv1alpha1.Workload) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(workload, workloadFinalizer) {
		return ctrl.Result{}, nil
	}

	key := types.NamespacedName{Name: workload.Name, Namespace: workload.Namespace}
	if err := r.notifyRemoved(ctx, key); err != nil {
		return ctrl.Result{RequeueAfter: statusRequeueInterval}, nil
	}

	controllerutil.RemoveFinalizer(workload, workloadFinalizer)
	if err := r.Update(ctx, workload); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing workload finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// setFailed marks the Workload as Failed and surfaces the reconcile error via
// a status condition (Reason=ReconcileError, the generic bucket — see
// setFailedWithReason for the handful of call sites that distinguish a
// specific, common failure mode instead), then returns it so the manager
// still applies its normal exponential-backoff requeue.
func (r *WorkloadReconciler) setFailed(ctx context.Context, workload *appsv1alpha1.Workload, reconcileErr error) (ctrl.Result, error) {
	return r.setFailedWithReason(ctx, workload, reasonReconcileError, reconcileErr)
}

// setFailedWithReason is setFailed with an explicit Condition Reason instead
// of always ReconcileError — used where the caller can already tell this
// failure apart from the generic bucket (e.g. render()'s
// errImageRequired/errUnknownTemplate) and that distinction is worth
// surfacing as its own translated message on the frontend rather than a
// generic "reconcile failed."
func (r *WorkloadReconciler) setFailedWithReason(ctx context.Context, workload *appsv1alpha1.Workload, reason string, reconcileErr error) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Error(reconcileErr, "reconcile failed")

	workload.Status.Phase = appsv1alpha1.PhaseFailed
	workload.Status.ObservedGeneration = workload.Generation
	apimeta.SetStatusCondition(&workload.Status.Conditions, metav1.Condition{
		Type:               appsv1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            reconcileErr.Error(),
		ObservedGeneration: workload.Generation,
	})
	if err := r.Status().Update(ctx, workload); err != nil {
		log.Error(err, "failed to update workload status after reconcile error")
	}

	// Fire-and-forget, same reasoning as notifyRemoved: this method's own
	// error return already triggers the manager's normal backoff-requeue,
	// which retries this call naturally on the next reconcile attempt if it
	// didn't land — there's no separate condition tracked for it the way
	// syncConvexLifecyclePhase tracks the success path.
	_ = r.notifyLifecycle(ctx, workload, lifecyclePhaseFailed, reconcileErr.Error(), false)

	return ctrl.Result{}, reconcileErr
}

// errImageRequired/errUnknownTemplate are sentinels render() wraps into its
// returned error (via %w) so Reconcile can tell these two common,
// user-caused misconfigurations apart via errors.Is and report a specific
// Condition Reason instead of the generic ReconcileError bucket — see
// reasonInvalidSpec/reasonUnknownTemplate.
var (
	errImageRequired   = errors.New("spec.image is required when spec.templateName is unset")
	errUnknownTemplate = errors.New("unknown template")
)

// render produces the containers/volumes/service-ports for workload, either
// by dispatching to a catalog template (when Spec.TemplateName is set) or by
// falling back to the original raw image/containerPort/env fields.
func (r *WorkloadReconciler) render(workload *appsv1alpha1.Workload) (catalog.Rendered, error) {
	if workload.Spec.TemplateName == "" {
		if workload.Spec.Image == "" {
			return catalog.Rendered{}, errImageRequired
		}
		return legacyRendered(workload), nil
	}

	tmpl, ok := catalog.Get(workload.Spec.TemplateName)
	if !ok {
		return catalog.Rendered{}, fmt.Errorf("%w %q", errUnknownTemplate, workload.Spec.TemplateName)
	}

	rawParams, err := configToParams(workload.Spec.Config)
	if err != nil {
		return catalog.Rendered{}, fmt.Errorf("parsing config: %w", err)
	}

	resolvedParams, err := catalog.ResolveParams(tmpl.Parameters, rawParams)
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

// desiredReplicaCount is the single source of truth for how many replicas
// the backing Deployment should run, shared by reconcileDeployment (what
// actually gets applied) and Reconcile's own status/phase calc — so the two
// can never silently diverge on suspend-awareness. A suspended workload
// always wants 0 replicas, overriding Spec.Replicas entirely; otherwise it's
// today's existing Spec.Replicas-defaulted-to-appsv1alpha1.DefaultReplicas logic.
func desiredReplicaCount(workload *appsv1alpha1.Workload) int32 {
	if workload.Spec.Suspended {
		return 0
	}
	if workload.Spec.Replicas != nil {
		return *workload.Spec.Replicas
	}
	return appsv1alpha1.DefaultReplicas
}

func (r *WorkloadReconciler) reconcileDeployment(ctx context.Context, workload *appsv1alpha1.Workload, selectorLabels, objectLabels map[string]string, rendered catalog.Rendered) error {
	replicas := desiredReplicaCount(workload)

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
		deployment.Spec.Template.Labels = objectLabels
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
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
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

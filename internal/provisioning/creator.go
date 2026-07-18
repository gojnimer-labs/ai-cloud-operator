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

// Package provisioning holds the Workload CR create/redeploy/destroy logic
// shared by internal/api's manual HTTP path (POST /workloads, DELETE
// /workloads/{name} — still reachable for local/manual testing) and
// internal/convexclient/runnable.go's claim-consumption loop (the normal
// flow once Convex owns create/destroy/redeploy as claimable requests
// rather than direct operator calls).
package provisioning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "github.com/gojnimer-labs/ai-cloud-operator/api/v1alpha1"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/catalog"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/labels"
)

// ErrUnknownTemplate is returned by Create/Redeploy when templateName isn't
// a registered catalog template — a caller-input error, not an internal
// one, so HTTP callers (internal/api) can map it to 400 rather than 500.
var ErrUnknownTemplate = errors.New("unknown template")

// ErrInvalidConfig is returned by Create/Redeploy when config fails the
// template's own parameter validation (catalog.ResolveParams) — likewise a
// caller-input error, not internal.
var ErrInvalidConfig = errors.New("invalid config")

// WorkloadCreator creates and redeploys Workload CRs in a single fixed
// namespace — the one WORKLOAD_NAMESPACE every workload this operator
// manages lives in (see cmd/main.go).
type WorkloadCreator struct {
	client    client.Client
	namespace string
}

// NewWorkloadCreator builds a WorkloadCreator that creates/redeploys
// Workload CRs in namespace using c.
func NewWorkloadCreator(c client.Client, namespace string) *WorkloadCreator {
	return &WorkloadCreator{client: c, namespace: namespace}
}

// Create builds and persists a brand-new Workload CR for templateName,
// exactly as internal/api's handleDeploy has always done: named via
// Kubernetes' own GenerateName (never a caller-supplied name), so multiple
// instances of the same template for the same user never collide.
//
// workloadID, when non-empty, is stamped onto the CR as the
// apps.aicloud.dev/workload-id label (see internal/labels) — the
// correlation token internal/convexclient/runnable.go's claim-consumption
// loop passes through from a claimed Convex row, letting the reconciler's
// first upsert report back which row this newly-generated name belongs to.
// The manual HTTP deploy path (internal/api) has no Convex row to
// correlate with, so it always passes an empty workloadID and no label is
// set.
func (wc *WorkloadCreator) Create(ctx context.Context, workloadID, templateName, userID, subdomain string, config map[string]any) (*appsv1alpha1.Workload, error) {
	tmpl, ok := catalog.Get(templateName)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTemplate, templateName)
	}
	if _, err := catalog.ResolveParams(tmpl.Parameters, config); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidConfig, err.Error())
	}

	configRaw, err := marshalConfig(config)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidConfig, err.Error())
	}

	workload := &appsv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: templateName + "-",
			Namespace:    wc.namespace,
		},
		Spec: appsv1alpha1.WorkloadSpec{
			TemplateName: templateName,
			UserID:       userID,
			Subdomain:    subdomain,
			Config:       configRaw,
		},
	}
	if workloadID != "" {
		workload.Labels = map[string]string{labels.WorkloadID: workloadID}
	}

	if err := wc.client.Create(ctx, workload); err != nil {
		return nil, fmt.Errorf("creating workload: %w", err)
	}
	return workload, nil
}

// Redeploy patches an existing Workload's Spec.Config to a new set of
// template parameter values and lets the reconciler take it from there — no
// Deployment/Service-patch logic needed here, since Reconcile already
// re-renders and applies both on every invocation regardless of what
// triggered it (see internal/controller/workload_controller.go's
// reconcileDeployment/reconcileService). Reuses the same
// param-resolution/validation Create uses, against the CR's own already-set
// TemplateName (redeploy never changes which template a workload runs).
//
// Also unconditionally bumps Spec.LastRedeployedAt to the current time —
// see that field's doc comment for why: without a guaranteed spec-level
// change on every call, a redeploy whose new config happens to be
// byte-identical to what's already stored is a true no-op Kubernetes API
// write (no resourceVersion/generation bump, no watch event), so the
// reconciler never runs again and never gets a chance to report the
// outcome back to Convex. This was observed live leaving a workload
// permanently stuck reporting "redeploying".
//
// Only Spec.Config and Spec.LastRedeployedAt are touched — Name, Namespace,
// TemplateName, UserID, Subdomain, and every label all stay exactly as they
// were.
func (wc *WorkloadCreator) Redeploy(ctx context.Context, name string, config map[string]any) error {
	var workload appsv1alpha1.Workload
	if err := wc.client.Get(ctx, client.ObjectKey{Namespace: wc.namespace, Name: name}, &workload); err != nil {
		return fmt.Errorf("getting workload %q: %w", name, err)
	}

	tmpl, ok := catalog.Get(workload.Spec.TemplateName)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownTemplate, workload.Spec.TemplateName)
	}
	if _, err := catalog.ResolveParams(tmpl.Parameters, config); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidConfig, err.Error())
	}

	configRaw, err := marshalConfig(config)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidConfig, err.Error())
	}
	workload.Spec.Config = configRaw
	workload.Spec.LastRedeployedAt = time.Now().UTC().Format(time.RFC3339Nano)

	if err := wc.client.Update(ctx, &workload); err != nil {
		return fmt.Errorf("updating workload %q: %w", name, err)
	}
	return nil
}

// SetSuspended patches an existing Workload's Spec.Suspended and lets the
// reconciler take it from there — same "no Deployment/Service-patch logic
// needed here" shape as Redeploy: Reconcile's desiredReplicaCount helper
// (internal/controller/workload_controller.go) picks up the change
// automatically on the resulting Update event, scaling the backing
// Deployment to 0 (suspended=true) or back to its normal replica count
// (suspended=false).
//
// Only Spec.Suspended is touched — Name, Namespace, TemplateName, Config,
// UserID, Subdomain, and every label all stay exactly as they were.
func (wc *WorkloadCreator) SetSuspended(ctx context.Context, name string, suspended bool) error {
	var workload appsv1alpha1.Workload
	if err := wc.client.Get(ctx, client.ObjectKey{Namespace: wc.namespace, Name: name}, &workload); err != nil {
		return fmt.Errorf("getting workload %q: %w", name, err)
	}

	workload.Spec.Suspended = suspended

	if err := wc.client.Update(ctx, &workload); err != nil {
		return fmt.Errorf("updating workload %q: %w", name, err)
	}
	return nil
}

// marshalConfig turns a resolved-parameter-shaped map into the CRD's loose
// JSON config bag — nil in, nil out (no parameters supplied is valid, not
// an error; see configToParams in internal/controller for the decode side).
func marshalConfig(config map[string]any) (*apiextensionsv1.JSON, error) {
	if config == nil {
		return nil, nil
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	return &apiextensionsv1.JSON{Raw: raw}, nil
}

// WorkloadDestroyer deletes Workload CRs in a single fixed namespace — see
// WorkloadCreator's doc comment for why the namespace is fixed rather than
// per-call.
type WorkloadDestroyer struct {
	client    client.Client
	namespace string
}

// NewWorkloadDestroyer builds a WorkloadDestroyer that deletes Workload CRs
// in namespace using c.
func NewWorkloadDestroyer(c client.Client, namespace string) *WorkloadDestroyer {
	return &WorkloadDestroyer{client: c, namespace: namespace}
}

// Destroy deletes the named Workload CR. A CR that's already gone is
// treated as success, not an error — Destroy is meant to be idempotent for
// both of its callers (a repeat manual DELETE, and the claim-consumption
// loop retrying a destroy that raced with an out-of-band kubectl delete).
func (wd *WorkloadDestroyer) Destroy(ctx context.Context, name string) error {
	workload := &appsv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: wd.namespace},
	}
	if err := wd.client.Delete(ctx, workload); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("deleting workload %q: %w", name, err)
	}
	return nil
}

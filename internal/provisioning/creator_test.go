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

// Package provisioning_test is deliberately an external test package (not
// "package provisioning"): TestReconcileAfterRedeployUpdatesDeployment below
// needs internal/controller, which imports internal/convexclient, which in
// turn imports this package (internal/provisioning) for the
// claim-consumption loop's WorkloadCreator/WorkloadDestroyer — an internal
// test file would make that an import cycle (provisioning -> controller ->
// convexclient -> provisioning). An external _test package sits outside
// that cycle entirely.
package provisioning_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1alpha1 "github.com/gojnimer-labs/ai-cloud-operator/api/v1alpha1"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/controller"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/labels"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/provisioning"
)

const (
	testNamespace       = "default"
	testTemplateID      = "nginx"
	testSubdomain       = "sub-1"
	testExistingName    = "nginx-abc"
	paramKeyWorkerConns = "workerConnections"
)

func newFakeClient(t *testing.T) (client.Client, *runtime.Scheme) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding core scheme: %v", err)
	}
	if err := appsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding workload scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1alpha1.Workload{}).Build()
	return c, scheme
}

func TestCreateSetsWorkloadIDLabelWhenPresent(t *testing.T) {
	c, _ := newFakeClient(t)
	creator := provisioning.NewWorkloadCreator(c, testNamespace)

	workload, err := creator.Create(context.Background(), "wl-123", testTemplateID, "user-1", testSubdomain, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := workload.Labels[labels.WorkloadID]; got != "wl-123" {
		t.Fatalf("expected %s=wl-123, got %q", labels.WorkloadID, got)
	}
	if workload.Spec.Subdomain != testSubdomain {
		t.Fatalf("expected subdomain to be set, got %q", workload.Spec.Subdomain)
	}
}

// TestCreateOmitsWorkloadIDLabelWhenEmpty is the concrete proof for the
// manual /workloads HTTP path (internal/api), which has no Convex row to
// correlate a create with and so always passes an empty workloadID.
func TestCreateOmitsWorkloadIDLabelWhenEmpty(t *testing.T) {
	c, _ := newFakeClient(t)
	creator := provisioning.NewWorkloadCreator(c, testNamespace)

	workload, err := creator.Create(context.Background(), "", testTemplateID, "user-1", "", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, ok := workload.Labels[labels.WorkloadID]; ok {
		t.Fatalf("expected no %s label when workloadID is empty, got %+v", labels.WorkloadID, workload.Labels)
	}
}

func TestCreateRejectsUnknownTemplate(t *testing.T) {
	c, _ := newFakeClient(t)
	creator := provisioning.NewWorkloadCreator(c, testNamespace)

	_, err := creator.Create(context.Background(), "", "does-not-exist", "user-1", "", nil)
	if !errors.Is(err, provisioning.ErrUnknownTemplate) {
		t.Fatalf("expected provisioning.ErrUnknownTemplate, got %v", err)
	}
}

func TestCreateRejectsInvalidConfig(t *testing.T) {
	c, _ := newFakeClient(t)
	creator := provisioning.NewWorkloadCreator(c, testNamespace)

	// workerConnections' Validation caps it at 65536 — 999999 must fail
	// ResolveParams and surface as provisioning.ErrInvalidConfig, not a k8s API error.
	_, err := creator.Create(context.Background(), "", testTemplateID, "user-1", "", map[string]any{paramKeyWorkerConns: float64(999999)})
	if !errors.Is(err, provisioning.ErrInvalidConfig) {
		t.Fatalf("expected provisioning.ErrInvalidConfig, got %v", err)
	}
}

// TestRedeployOnlyTouchesSpecConfig is the concrete proof of the plan's
// "Redeploy needs no new Deployment/Service-patching logic" claim's
// prerequisite: Redeploy itself must only ever change Spec.Config, leaving
// Name and every other field (including labels) exactly as they were.
func TestRedeployOnlyTouchesSpecConfig(t *testing.T) {
	c, _ := newFakeClient(t)
	creator := provisioning.NewWorkloadCreator(c, testNamespace)

	original := &appsv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testExistingName,
			Namespace: testNamespace,
			Labels:    map[string]string{"keep": "me"},
		},
		Spec: appsv1alpha1.WorkloadSpec{
			TemplateName: testTemplateID,
			UserID:       "user-1",
			Subdomain:    testSubdomain,
		},
	}
	if err := c.Create(context.Background(), original); err != nil {
		t.Fatalf("seeding workload: %v", err)
	}

	if err := creator.Redeploy(context.Background(), testExistingName, map[string]any{paramKeyWorkerConns: float64(2048)}); err != nil {
		t.Fatalf("redeploy: %v", err)
	}

	var updated appsv1alpha1.Workload
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testExistingName}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Name != testExistingName || updated.Spec.UserID != "user-1" || updated.Spec.Subdomain != testSubdomain || updated.Spec.TemplateName != testTemplateID {
		t.Fatalf("expected only Spec.Config to change, got %+v", updated.Spec)
	}
	if updated.Labels["keep"] != "me" {
		t.Fatalf("expected existing labels to be untouched, got %+v", updated.Labels)
	}
	if updated.Spec.Config == nil {
		t.Fatalf("expected Spec.Config to be set")
	}
	var gotConfig map[string]any
	if err := json.Unmarshal(updated.Spec.Config.Raw, &gotConfig); err != nil {
		t.Fatalf("unmarshaling config: %v", err)
	}
	if gotConfig[paramKeyWorkerConns] != float64(2048) {
		t.Fatalf("expected workerConnections=2048, got %+v", gotConfig)
	}
}

func TestRedeployRejectsInvalidConfig(t *testing.T) {
	c, _ := newFakeClient(t)
	creator := provisioning.NewWorkloadCreator(c, testNamespace)

	original := &appsv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: testExistingName, Namespace: testNamespace},
		Spec:       appsv1alpha1.WorkloadSpec{TemplateName: testTemplateID},
	}
	if err := c.Create(context.Background(), original); err != nil {
		t.Fatalf("seeding workload: %v", err)
	}

	err := creator.Redeploy(context.Background(), testExistingName, map[string]any{paramKeyWorkerConns: float64(999999)})
	if !errors.Is(err, provisioning.ErrInvalidConfig) {
		t.Fatalf("expected provisioning.ErrInvalidConfig, got %v", err)
	}
}

func TestDestroySwallowsNotFound(t *testing.T) {
	c, _ := newFakeClient(t)
	destroyer := provisioning.NewWorkloadDestroyer(c, testNamespace)

	if err := destroyer.Destroy(context.Background(), "does-not-exist"); err != nil {
		t.Fatalf("expected nil error for an already-gone workload, got %v", err)
	}
}

func TestDestroyDeletesExistingWorkload(t *testing.T) {
	c, _ := newFakeClient(t)
	destroyer := provisioning.NewWorkloadDestroyer(c, testNamespace)

	workload := &appsv1alpha1.Workload{ObjectMeta: metav1.ObjectMeta{Name: testExistingName, Namespace: testNamespace}}
	if err := c.Create(context.Background(), workload); err != nil {
		t.Fatalf("seeding workload: %v", err)
	}

	if err := destroyer.Destroy(context.Background(), testExistingName); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	var check appsv1alpha1.Workload
	err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testExistingName}, &check)
	if err == nil {
		t.Fatalf("expected workload to have been deleted")
	}
}

// TestReconcileAfterRedeployUpdatesDeployment is the concrete, repo-local
// proof of the plan's central Redeploy claim: Reconcile already re-renders
// and applies the Deployment from Spec.Config on every invocation (via
// controllerutil.CreateOrUpdate in reconcileDeployment), so Redeploy's own
// job really is just patching Spec.Config — no separate Deployment-patch
// logic is needed for a config change to actually roll out.
func TestReconcileAfterRedeployUpdatesDeployment(t *testing.T) {
	c, scheme := newFakeClient(t)
	creator := provisioning.NewWorkloadCreator(c, testNamespace)

	workload, err := creator.Create(context.Background(), "", testTemplateID, "user-1", "", map[string]any{paramKeyWorkerConns: float64(1024)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	reconciler := &controller.WorkloadReconciler{Client: c, Scheme: scheme}
	nsName := client.ObjectKey{Namespace: testNamespace, Name: workload.Name}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: nsName}); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}

	var deployment appsv1.Deployment
	if err := c.Get(context.Background(), nsName, &deployment); err != nil {
		t.Fatalf("getting deployment: %v", err)
	}
	if got := workerConnectionsEnv(t, deployment); got != "1024" {
		t.Fatalf("expected initial WORKER_CONNECTIONS=1024, got %q", got)
	}

	if err := creator.Redeploy(context.Background(), workload.Name, map[string]any{paramKeyWorkerConns: float64(4096)}); err != nil {
		t.Fatalf("redeploy: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: nsName}); err != nil {
		t.Fatalf("post-redeploy reconcile: %v", err)
	}

	if err := c.Get(context.Background(), nsName, &deployment); err != nil {
		t.Fatalf("getting deployment after redeploy: %v", err)
	}
	if got := workerConnectionsEnv(t, deployment); got != "4096" {
		t.Fatalf("expected redeployed WORKER_CONNECTIONS=4096, got %q", got)
	}
}

func workerConnectionsEnv(t *testing.T, deployment appsv1.Deployment) string {
	t.Helper()
	if len(deployment.Spec.Template.Spec.Containers) == 0 {
		t.Fatalf("expected at least one container")
	}
	for _, env := range deployment.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "WORKER_CONNECTIONS" {
			return env.Value
		}
	}
	t.Fatalf("WORKER_CONNECTIONS env var not found")
	return ""
}

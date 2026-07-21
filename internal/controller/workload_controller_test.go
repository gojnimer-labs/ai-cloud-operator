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
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1alpha1 "github.com/gojnimer-labs/ai-cloud-operator/api/v1alpha1"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/convexclient"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/labels"
)

const testImage = "nginx:latest"

// lifecycleReport records one WorkloadNotifier.ReportLifecycle call, for
// assertions in fakeNotifier's consumers.
type lifecycleReport struct {
	name       string
	workloadID string
	phase      string
	reason     string
	retryable  bool
}

// fakeNotifier records UpsertWorkload/RemoveWorkload/ReportLifecycle calls
// for assertions, standing in for a real *convexclient.Runnable. upsertErr,
// when set, makes every UpsertWorkload call fail (while still recording it)
// — used to exercise syncConvex's retry-until-success path. lifecycleErr
// does the same for ReportLifecycle.
type fakeNotifier struct {
	mu           sync.Mutex
	upserts      []convexclient.WorkloadInfo
	removals     []types.NamespacedName
	lifecycles   []lifecycleReport
	upsertErr    error
	lifecycleErr error
	removeErr    error
}

func (f *fakeNotifier) UpsertWorkload(_ context.Context, info convexclient.WorkloadInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts = append(f.upserts, info)
	return f.upsertErr
}

func (f *fakeNotifier) RemoveWorkload(_ context.Context, name, namespace string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removals = append(f.removals, types.NamespacedName{Name: name, Namespace: namespace})
	return f.removeErr
}

func (f *fakeNotifier) ReportLifecycle(_ context.Context, name, workloadID, phase, reason string, retryable bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lifecycles = append(f.lifecycles, lifecycleReport{name: name, workloadID: workloadID, phase: phase, reason: reason, retryable: retryable})
	return f.lifecycleErr
}

func (f *fakeNotifier) upsertCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.upserts)
}

func (f *fakeNotifier) removalCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.removals)
}

func (f *fakeNotifier) lifecycleCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.lifecycles)
}

func (f *fakeNotifier) lastLifecycle() lifecycleReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.lifecycles) == 0 {
		return lifecycleReport{}
	}
	return f.lifecycles[len(f.lifecycles)-1]
}

// forceRemoveFinalizers is test cleanup only — production finalizer removal
// always goes through WorkloadReconciler.reconcileDelete's
// notify-then-release path. Every AfterEach below deletes its Workload
// without a follow-up Reconcile call, and workloadFinalizer now blocks a
// bare k8sClient.Delete from actually removing the object (it only sets
// DeletionTimestamp) — this strips the finalizer directly so cleanup
// between tests behaves the same as it did before the finalizer existed.
// A no-op if the object is already gone or already finalizer-free.
func forceRemoveFinalizers(ctx context.Context, key types.NamespacedName) {
	var workload appsv1alpha1.Workload
	if err := k8sClient.Get(ctx, key, &workload); err != nil {
		return
	}
	if len(workload.Finalizers) == 0 {
		return
	}
	workload.Finalizers = nil
	_ = k8sClient.Update(ctx, &workload)
}

var _ = Describe("Workload Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		workload := &appsv1alpha1.Workload{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Workload")
			err := k8sClient.Get(ctx, typeNamespacedName, workload)
			if err != nil && errors.IsNotFound(err) {
				resource := &appsv1alpha1.Workload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: appsv1alpha1.WorkloadSpec{
						Image: testImage,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &appsv1alpha1.Workload{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Workload")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			forceRemoveFinalizers(ctx, typeNamespacedName)
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &WorkloadReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("adding workloadFinalizer in the same pass")
			var reconciled appsv1alpha1.Workload
			Expect(k8sClient.Get(ctx, typeNamespacedName, &reconciled)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(&reconciled, workloadFinalizer)).To(BeTrue())
		})

	})

	// Separate Context with its own resource name: envtest runs only
	// kube-apiserver+etcd, not the garbage-collector controller, so
	// owner-reference cascade deletion never actually happens here — a
	// Deployment/Service from another test's resource name would otherwise
	// linger with a stale selector (immutable) and collide with this one.
	Context("When a Workload specifies a UserID", func() {
		const (
			resourceName      = "test-resource-userid"
			resourceNamespace = "default"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}

		AfterEach(func() {
			workload := &appsv1alpha1.Workload{}
			if err := k8sClient.Get(ctx, typeNamespacedName, workload); err == nil {
				Expect(k8sClient.Delete(ctx, workload)).To(Succeed())
				forceRemoveFinalizers(ctx, typeNamespacedName)
			}
			Expect(k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
			Expect(k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
		})

		It("applies a valid Spec.UserID as a label on object metadata but not the selector", func() {
			resource := &appsv1alpha1.Workload{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec:       appsv1alpha1.WorkloadSpec{Image: testImage, UserID: "user-123"},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			controllerReconciler := &WorkloadReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			var deployment appsv1.Deployment
			Expect(k8sClient.Get(ctx, typeNamespacedName, &deployment)).To(Succeed())
			Expect(deployment.Labels).To(HaveKeyWithValue(labelUserID, "user-123"))
			Expect(deployment.Spec.Selector.MatchLabels).NotTo(HaveKey(labelUserID))
			Expect(deployment.Spec.Template.ObjectMeta.Labels).To(HaveKeyWithValue(labelUserID, "user-123"))

			var service corev1.Service
			Expect(k8sClient.Get(ctx, typeNamespacedName, &service)).To(Succeed())
			Expect(service.Labels).To(HaveKeyWithValue(labelUserID, "user-123"))
			Expect(service.Spec.Selector).NotTo(HaveKey(labelUserID))
		})
	})

	Context("When a WorkloadNotifier is configured", func() {
		const (
			resourceName      = "test-resource-notify"
			resourceNamespace = "default"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}

		AfterEach(func() {
			workload := &appsv1alpha1.Workload{}
			if err := k8sClient.Get(ctx, typeNamespacedName, workload); err == nil {
				Expect(k8sClient.Delete(ctx, workload)).To(Succeed())
				forceRemoveFinalizers(ctx, typeNamespacedName)
			}
			Expect(k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
			Expect(k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
		})

		It("upserts once on creation, not again on an unchanged reconcile, and removes on deletion", func() {
			resource := &appsv1alpha1.Workload{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: resourceNamespace,
					Labels:    map[string]string{labels.WorkloadID: "wl-789"},
				},
				Spec: appsv1alpha1.WorkloadSpec{
					Image:        testImage,
					Subdomain:    "demo-sub",
					TemplateName: "nginx",
					UserID:       "user-456",
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			notifier := &fakeNotifier{}
			controllerReconciler := &WorkloadReconciler{
				Client:       k8sClient,
				ConvexClient: notifier,
				Scheme:       k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(notifier.upsertCount()).To(Equal(1))
			Expect(notifier.upserts[0].Name).To(Equal(resourceName))
			Expect(notifier.upserts[0].TemplateName).To(Equal("nginx"))
			Expect(notifier.upserts[0].UserID).To(Equal("user-456"))
			Expect(notifier.upserts[0].Subdomain).To(Equal("demo-sub"))
			// B6: the claim-flow correlation label, when present, must ride
			// along in the upsert so Convex's record mutation can resolve
			// this workload directly by _id for its very first sync.
			Expect(notifier.upserts[0].WorkloadID).To(Equal("wl-789"))

			// A second reconcile with no spec change must not upsert again.
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(notifier.upsertCount()).To(Equal(1))

			// Deleting the CR and reconciling again must trigger removal.
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(notifier.removalCount()).To(Equal(1))
			Expect(notifier.removals[0]).To(Equal(typeNamespacedName))

			// workloadFinalizer only releases once the notify above
			// succeeds — confirm the object is actually gone, not just
			// marked DeletionTimestamp, in this single reconcile pass.
			err = k8sClient.Get(ctx, typeNamespacedName, &appsv1alpha1.Workload{})
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("When Convex notify fails during deletion", func() {
		const (
			resourceName      = "test-resource-notify-delete-retry"
			resourceNamespace = "default"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}

		AfterEach(func() {
			workload := &appsv1alpha1.Workload{}
			if err := k8sClient.Get(ctx, typeNamespacedName, workload); err == nil {
				Expect(k8sClient.Delete(ctx, workload)).To(Succeed())
				forceRemoveFinalizers(ctx, typeNamespacedName)
			}
			Expect(k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
			Expect(k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
		})

		It("keeps workloadFinalizer and retries on every reconcile until the removal notify succeeds", func() {
			resource := &appsv1alpha1.Workload{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec:       appsv1alpha1.WorkloadSpec{Image: testImage},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			notifier := &fakeNotifier{removeErr: fmt.Errorf("simulated convex outage")}
			controllerReconciler := &WorkloadReconciler{
				Client:       k8sClient,
				ConvexClient: notifier,
				Scheme:       k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(statusRequeueInterval))
			Expect(notifier.removalCount()).To(Equal(1))

			// Object must still exist, still finalized, still Terminating —
			// a failed notify must never let deletion complete.
			var stillThere appsv1alpha1.Workload
			Expect(k8sClient.Get(ctx, typeNamespacedName, &stillThere)).To(Succeed())
			Expect(stillThere.DeletionTimestamp.IsZero()).To(BeFalse())
			Expect(controllerutil.ContainsFinalizer(&stillThere, workloadFinalizer)).To(BeTrue())

			// Once Convex is reachable again, the next reconcile releases
			// the finalizer and the object is actually gone.
			notifier.mu.Lock()
			notifier.removeErr = nil
			notifier.mu.Unlock()

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(notifier.removalCount()).To(Equal(2))

			err = k8sClient.Get(ctx, typeNamespacedName, &appsv1alpha1.Workload{})
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("When no WorkloadNotifier is configured", func() {
		const (
			resourceName      = "test-resource-no-notifier-delete"
			resourceNamespace = "default"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}

		AfterEach(func() {
			workload := &appsv1alpha1.Workload{}
			if err := k8sClient.Get(ctx, typeNamespacedName, workload); err == nil {
				Expect(k8sClient.Delete(ctx, workload)).To(Succeed())
				forceRemoveFinalizers(ctx, typeNamespacedName)
			}
			Expect(k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
			Expect(k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
		})

		It("still adds and releases workloadFinalizer cleanly with ConvexClient nil", func() {
			resource := &appsv1alpha1.Workload{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec:       appsv1alpha1.WorkloadSpec{Image: testImage},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			controllerReconciler := &WorkloadReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var reconciled appsv1alpha1.Workload
			Expect(k8sClient.Get(ctx, typeNamespacedName, &reconciled)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(&reconciled, workloadFinalizer)).To(BeTrue())

			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, typeNamespacedName, &appsv1alpha1.Workload{})
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("When Convex notify fails", func() {
		const (
			resourceName      = "test-resource-notify-retry"
			resourceNamespace = "default"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}

		AfterEach(func() {
			workload := &appsv1alpha1.Workload{}
			if err := k8sClient.Get(ctx, typeNamespacedName, workload); err == nil {
				Expect(k8sClient.Delete(ctx, workload)).To(Succeed())
				forceRemoveFinalizers(ctx, typeNamespacedName)
			}
			Expect(k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
			Expect(k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
		})

		It("marks ConvexSynced False and keeps retrying on every reconcile until it succeeds", func() {
			resource := &appsv1alpha1.Workload{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec:       appsv1alpha1.WorkloadSpec{Image: testImage},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			notifier := &fakeNotifier{upsertErr: fmt.Errorf("simulated convex outage")}
			controllerReconciler := &WorkloadReconciler{
				Client:       k8sClient,
				ConvexClient: notifier,
				Scheme:       k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			Expect(notifier.upsertCount()).To(Equal(1))

			var workload appsv1alpha1.Workload
			Expect(k8sClient.Get(ctx, typeNamespacedName, &workload)).To(Succeed())
			cond := apimeta.FindStatusCondition(workload.Status.Conditions, conditionTypeConvexSynced)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))

			// A follow-up reconcile with no spec change must still
			// re-attempt Convex sync, since the last attempt didn't
			// succeed — this is the retry-until-success fix.
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(notifier.upsertCount()).To(Equal(2))

			// Once Convex is reachable again, the next reconcile succeeds
			// and ConvexSynced flips True.
			notifier.mu.Lock()
			notifier.upsertErr = nil
			notifier.mu.Unlock()
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(notifier.upsertCount()).To(Equal(3))

			Expect(k8sClient.Get(ctx, typeNamespacedName, &workload)).To(Succeed())
			cond = apimeta.FindStatusCondition(workload.Status.Conditions, conditionTypeConvexSynced)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))

			// A further reconcile with nothing changed must not
			// re-attempt — mirrors the existing "not again on an unchanged
			// reconcile" expectation above.
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(notifier.upsertCount()).To(Equal(3))
		})
	})

	Context("When a Workload reaches Running", func() {
		const (
			resourceName      = "test-resource-lifecycle-active"
			resourceNamespace = "default"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}

		AfterEach(func() {
			workload := &appsv1alpha1.Workload{}
			if err := k8sClient.Get(ctx, typeNamespacedName, workload); err == nil {
				Expect(k8sClient.Delete(ctx, workload)).To(Succeed())
				forceRemoveFinalizers(ctx, typeNamespacedName)
			}
			Expect(k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
			Expect(k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
		})

		It("reports active once, not again on a subsequent Running reconcile", func() {
			resource := &appsv1alpha1.Workload{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec:       appsv1alpha1.WorkloadSpec{Image: testImage},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			notifier := &fakeNotifier{}
			controllerReconciler := &WorkloadReconciler{
				Client:       k8sClient,
				ConvexClient: notifier,
				Scheme:       k8sClient.Scheme(),
			}

			// First reconcile: the Deployment gets created but envtest runs
			// no real kubelet/deployment-controller, so ReadyReplicas stays
			// 0 — phase is Deploying, not yet Running, so no active report
			// is attempted yet.
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(notifier.lifecycleCount()).To(Equal(0))

			// Simulate the Deployment actually becoming ready. Status.Replicas
			// must be set too — the apiserver rejects readyReplicas >
			// replicas.
			var deployment appsv1.Deployment
			Expect(k8sClient.Get(ctx, typeNamespacedName, &deployment)).To(Succeed())
			deployment.Status.Replicas = 1
			deployment.Status.ReadyReplicas = 1
			Expect(k8sClient.Status().Update(ctx, &deployment)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(notifier.lifecycleCount()).To(Equal(1))
			report := notifier.lastLifecycle()
			Expect(report.name).To(Equal(resourceName))
			Expect(report.phase).To(Equal(lifecyclePhaseActive))

			var workload appsv1alpha1.Workload
			Expect(k8sClient.Get(ctx, typeNamespacedName, &workload)).To(Succeed())
			Expect(workload.Status.Phase).To(Equal(appsv1alpha1.PhaseRunning))
			cond := apimeta.FindStatusCondition(workload.Status.Conditions, conditionTypeConvexLifecycleSynced)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))

			// A further reconcile with nothing changed must not re-report.
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(notifier.lifecycleCount()).To(Equal(1))
		})
	})

	// Regression coverage for a real stuck-forever loop observed in
	// production: a workload that hit a transient reconcileDeployment
	// conflict at create time got its "failed" report reinterpreted by
	// Convex as "active" (ai-cloud-v2's resolveLifecycleStatus), which left
	// the row no longer in an in-flight status. The next, genuine "active"
	// report then permanently 409'd as "stale" — with no special-casing,
	// that requeued every statusRequeueInterval forever, generating
	// unbounded ERROR logs and Convex traffic for a workload that was, in
	// fact, perfectly healthy. Its own Context (rather than reusing "When a
	// Workload reaches Running" above) because that Context's AfterEach
	// unconditionally deletes a fixed-name Deployment/Service this test
	// never creates under.
	Context("When Convex reports a workload as already resolved (stale 409)", func() {
		const (
			resourceName      = "test-resource-lifecycle-stale"
			resourceNamespace = "default"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}

		AfterEach(func() {
			workload := &appsv1alpha1.Workload{}
			if err := k8sClient.Get(ctx, typeNamespacedName, workload); err == nil {
				Expect(k8sClient.Delete(ctx, workload)).To(Succeed())
				forceRemoveFinalizers(ctx, typeNamespacedName)
			}
			Expect(k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
			Expect(k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
		})

		It("settles ConvexLifecycleSynced instead of retrying forever", func() {
			resource := &appsv1alpha1.Workload{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec:       appsv1alpha1.WorkloadSpec{Image: testImage},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			notifier := &fakeNotifier{lifecycleErr: convexclient.ErrLifecycleStale}
			controllerReconciler := &WorkloadReconciler{
				Client:       k8sClient,
				ConvexClient: notifier,
				Scheme:       k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(notifier.lifecycleCount()).To(Equal(0))

			var deployment appsv1.Deployment
			Expect(k8sClient.Get(ctx, typeNamespacedName, &deployment)).To(Succeed())
			deployment.Status.Replicas = 1
			deployment.Status.ReadyReplicas = 1
			Expect(k8sClient.Status().Update(ctx, &deployment)).To(Succeed())

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(notifier.lifecycleCount()).To(Equal(1))
			// The whole point: a stale 409 must not schedule another
			// attempt against a report that can never succeed.
			Expect(result.RequeueAfter).To(BeZero())

			var workload appsv1alpha1.Workload
			Expect(k8sClient.Get(ctx, typeNamespacedName, &workload)).To(Succeed())
			cond := apimeta.FindStatusCondition(workload.Status.Conditions, conditionTypeConvexLifecycleSynced)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("AlreadyResolved"))

			// A further reconcile with nothing changed must not retry —
			// this is the unbounded-loop this test guards against.
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(notifier.lifecycleCount()).To(Equal(1))
		})
	})

	// Regression coverage for the actual root cause behind the stale-409
	// scenario above: a resourceVersion conflict from reconcileDeployment/
	// reconcileService's CreateOrUpdate (e.g. another concurrent reconcile of
	// this same Workload, or a human deleting/recreating the Deployment
	// out-of-band via kubectl/Headlamp) is routine and self-correcting on the
	// very next reconcile — it must never be treated as a Workload failure or
	// reported to Convex at all.
	Context("When reconcileDeployment hits a resourceVersion conflict", func() {
		const (
			resourceName      = "test-resource-conflict"
			resourceNamespace = "default"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}

		AfterEach(func() {
			workload := &appsv1alpha1.Workload{}
			if err := k8sClient.Get(ctx, typeNamespacedName, workload); err == nil {
				Expect(k8sClient.Delete(ctx, workload)).To(Succeed())
				forceRemoveFinalizers(ctx, typeNamespacedName)
			}
			Expect(k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
			Expect(k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
		})

		It("requeues without calling setFailed or notifying Convex", func() {
			resource := &appsv1alpha1.Workload{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec:       appsv1alpha1.WorkloadSpec{Image: testImage},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			notifier := &fakeNotifier{}
			// k8sClient is a plain client.New (no Watch support), but
			// interceptor.NewClient requires client.WithWatch — build a
			// second client against the same envtest apiserver instead of
			// type-asserting k8sClient itself.
			watchClient, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			conflictingClient := interceptor.NewClient(watchClient, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*appsv1.Deployment); ok {
						return errors.NewConflict(
							schema.GroupResource{Group: "apps", Resource: "deployments"},
							obj.GetName(),
							fmt.Errorf("the object has been modified; please apply your changes to the latest version and try again"),
						)
					}
					return c.Update(ctx, obj, opts...)
				},
			})
			controllerReconciler := &WorkloadReconciler{
				Client:       conflictingClient,
				ConvexClient: notifier,
				Scheme:       k8sClient.Scheme(),
			}

			// First reconcile creates the Deployment fresh (no Update
			// involved yet, so the interceptor doesn't fire).
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// Force an actual spec diff so CreateOrUpdate's semantic-equality
			// check decides an Update is needed on the next reconcile —
			// otherwise it may see the Deployment already matches and never
			// call Update at all, and the interceptor below would never fire.
			var workload appsv1alpha1.Workload
			Expect(k8sClient.Get(ctx, typeNamespacedName, &workload)).To(Succeed())
			two := int32(2)
			workload.Spec.Replicas = &two
			Expect(k8sClient.Update(ctx, &workload)).To(Succeed())

			// Second reconcile hits CreateOrUpdate's Update path (the
			// Deployment already exists and now needs its replica count
			// changed) — the interceptor forces a conflict here.
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(errors.IsConflict(err)).To(BeTrue())
			Expect(notifier.lifecycleCount()).To(Equal(0))

			Expect(k8sClient.Get(ctx, typeNamespacedName, &workload)).To(Succeed())
			Expect(workload.Status.Phase).NotTo(Equal(appsv1alpha1.PhaseFailed))
		})
	})

	Context("When a Workload's Pod is stuck Unschedulable", func() {
		const (
			resourceName      = "test-resource-unschedulable"
			resourceNamespace = "default"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}

		AfterEach(func() {
			workload := &appsv1alpha1.Workload{}
			if err := k8sClient.Get(ctx, typeNamespacedName, workload); err == nil {
				Expect(k8sClient.Delete(ctx, workload)).To(Succeed())
				forceRemoveFinalizers(ctx, typeNamespacedName)
			}
			Expect(k8sClient.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(resourceNamespace))).To(Succeed())
			_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})
			_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})
		})

		// stuckPod creates a Pod carrying workload's selector labels (so
		// checkUnschedulable's List(MatchingLabels) finds it) with a
		// PodScheduled=False/Unschedulable condition transitioned at
		// transitionedAt.
		stuckPod := func(workload *appsv1alpha1.Workload, transitionedAt time.Time) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: resourceName + "-",
					Namespace:    resourceNamespace,
					Labels: map[string]string{
						labelName:        workload.Name,
						labelInstance:    string(workload.UID),
						labels.ManagedBy: labels.ManagedByValue,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: testImage}},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			pod.Status.Conditions = []corev1.PodCondition{{
				LastTransitionTime: metav1.NewTime(transitionedAt),
				Message:            "0/3 nodes are available: 3 Insufficient cpu.",
				Reason:             corev1.PodReasonUnschedulable,
				Status:             corev1.ConditionFalse,
				Type:               corev1.PodScheduled,
			}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		}

		It("releases the claim (retryable) and deletes the Workload once the grace period elapses", func() {
			resource := &appsv1alpha1.Workload{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec:       appsv1alpha1.WorkloadSpec{Image: testImage},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			notifier := &fakeNotifier{}
			controllerReconciler := &WorkloadReconciler{
				Client:       k8sClient,
				ConvexClient: notifier,
				Scheme:       k8sClient.Scheme(),
			}

			// First reconcile creates the Deployment/Service; envtest runs no
			// real scheduler, so no Pod exists yet — nothing to detect.
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(notifier.lifecycleCount()).To(Equal(0))

			var workload appsv1alpha1.Workload
			Expect(k8sClient.Get(ctx, typeNamespacedName, &workload)).To(Succeed())
			stuckPod(&workload, time.Now().Add(-(unschedulableGracePeriod + time.Minute)))

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			Expect(notifier.lifecycleCount()).To(Equal(1))
			report := notifier.lastLifecycle()
			Expect(report.phase).To(Equal(lifecyclePhaseFailed))
			Expect(report.retryable).To(BeTrue())
			Expect(report.reason).To(ContainSubstring("insufficient cluster capacity"))

			// Delete only sets DeletionTimestamp (workloadFinalizer holds it
			// open) — confirms release-then-delete actually fired.
			Expect(k8sClient.Get(ctx, typeNamespacedName, &workload)).To(Succeed())
			Expect(workload.DeletionTimestamp.IsZero()).To(BeFalse())
		})

		It("does not release the claim while still within the grace period", func() {
			resource := &appsv1alpha1.Workload{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec:       appsv1alpha1.WorkloadSpec{Image: testImage},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			notifier := &fakeNotifier{}
			controllerReconciler := &WorkloadReconciler{
				Client:       k8sClient,
				ConvexClient: notifier,
				Scheme:       k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var workload appsv1alpha1.Workload
			Expect(k8sClient.Get(ctx, typeNamespacedName, &workload)).To(Succeed())
			stuckPod(&workload, time.Now().Add(-30*time.Second))

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(notifier.lifecycleCount()).To(Equal(0))

			Expect(k8sClient.Get(ctx, typeNamespacedName, &workload)).To(Succeed())
			Expect(workload.DeletionTimestamp.IsZero()).To(BeTrue())
		})
	})

	Context("When a Workload is suspended", func() {
		const (
			resourceName      = "test-resource-suspend"
			resourceNamespace = "default"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}

		AfterEach(func() {
			workload := &appsv1alpha1.Workload{}
			if err := k8sClient.Get(ctx, typeNamespacedName, workload); err == nil {
				Expect(k8sClient.Delete(ctx, workload)).To(Succeed())
				forceRemoveFinalizers(ctx, typeNamespacedName)
			}
			Expect(k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
			Expect(k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace}})).To(Succeed())
		})

		It("scales the Deployment to 0 and sets Phase Stopped, then rolls back to Running when un-suspended", func() {
			resource := &appsv1alpha1.Workload{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec:       appsv1alpha1.WorkloadSpec{Image: testImage},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			notifier := &fakeNotifier{}
			controllerReconciler := &WorkloadReconciler{
				Client:       k8sClient,
				ConvexClient: notifier,
				Scheme:       k8sClient.Scheme(),
			}

			// Get to Running first, same as the "reaches Running" test above.
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var deployment appsv1.Deployment
			Expect(k8sClient.Get(ctx, typeNamespacedName, &deployment)).To(Succeed())
			deployment.Status.Replicas = 1
			deployment.Status.ReadyReplicas = 1
			Expect(k8sClient.Status().Update(ctx, &deployment)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var workload appsv1alpha1.Workload
			Expect(k8sClient.Get(ctx, typeNamespacedName, &workload)).To(Succeed())
			Expect(workload.Status.Phase).To(Equal(appsv1alpha1.PhaseRunning))

			// Suspend: Spec.Suspended=true bumps Generation. reconcileDeployment
			// must scale the Deployment down to 0 replicas immediately, even
			// before ReadyReplicas has caught up.
			workload.Spec.Suspended = true
			Expect(k8sClient.Update(ctx, &workload)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, typeNamespacedName, &deployment)).To(Succeed())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(0)))

			// Simulate the Deployment actually finishing its scale-down to 0
			// ready replicas (envtest runs no real deployment controller).
			deployment.Status.Replicas = 0
			deployment.Status.ReadyReplicas = 0
			Expect(k8sClient.Status().Update(ctx, &deployment)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, typeNamespacedName, &workload)).To(Succeed())
			Expect(workload.Status.Phase).To(Equal(appsv1alpha1.PhaseStopped))

			report := notifier.lastLifecycle()
			Expect(report.phase).To(Equal(lifecyclePhaseStopped))

			// Un-suspend: Spec.Suspended=false bumps Generation again, rolling
			// the Deployment back up to its normal replica count.
			workload.Spec.Suspended = false
			Expect(k8sClient.Update(ctx, &workload)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, typeNamespacedName, &deployment)).To(Succeed())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(1)))

			// Simulate the Deployment scaling back up.
			deployment.Status.Replicas = 1
			deployment.Status.ReadyReplicas = 1
			Expect(k8sClient.Status().Update(ctx, &deployment)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, typeNamespacedName, &workload)).To(Succeed())
			Expect(workload.Status.Phase).To(Equal(appsv1alpha1.PhaseRunning))

			report = notifier.lastLifecycle()
			Expect(report.phase).To(Equal(lifecyclePhaseActive))
		})
	})

	Context("When reconcile fails before ever reaching Running", func() {
		const (
			resourceName      = "test-resource-lifecycle-failed"
			resourceNamespace = "default"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}

		AfterEach(func() {
			workload := &appsv1alpha1.Workload{}
			if err := k8sClient.Get(ctx, typeNamespacedName, workload); err == nil {
				Expect(k8sClient.Delete(ctx, workload)).To(Succeed())
				forceRemoveFinalizers(ctx, typeNamespacedName)
			}
		})

		It("calls ReportLifecycle(failed) from setFailed, carrying the workload-id label when present", func() {
			// No Image and no TemplateName: render() errors immediately, so
			// Reconcile never gets anywhere near the Deployment/Service or
			// the appsv1alpha1.PhaseRunning block — setFailed is the only call site that
			// can possibly run here, proving active/failed really are two
			// distinct call sites, not a shared helper.
			resource := &appsv1alpha1.Workload{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: resourceNamespace,
					Labels:    map[string]string{labels.WorkloadID: "wl-failed-1"},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			notifier := &fakeNotifier{}
			controllerReconciler := &WorkloadReconciler{
				Client:       k8sClient,
				ConvexClient: notifier,
				Scheme:       k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).To(HaveOccurred())

			Expect(notifier.lifecycleCount()).To(Equal(1))
			report := notifier.lastLifecycle()
			Expect(report.name).To(Equal(resourceName))
			Expect(report.workloadID).To(Equal("wl-failed-1"))
			Expect(report.phase).To(Equal(lifecyclePhaseFailed))
			Expect(report.reason).NotTo(BeEmpty())

			var workload appsv1alpha1.Workload
			Expect(k8sClient.Get(ctx, typeNamespacedName, &workload)).To(Succeed())
			Expect(workload.Status.Phase).To(Equal(appsv1alpha1.PhaseFailed))
		})
	})
})

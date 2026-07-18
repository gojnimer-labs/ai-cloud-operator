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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
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
	return nil
}

func (f *fakeNotifier) ReportLifecycle(_ context.Context, name, workloadID, phase, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lifecycles = append(f.lifecycles, lifecycleReport{name: name, workloadID: workloadID, phase: phase, reason: reason})
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
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
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

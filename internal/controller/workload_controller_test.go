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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1alpha1 "github.com/gojnimer-labs/ai-cloud-operator/api/v1alpha1"
)

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
						Image: "nginx:latest",
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
				Spec:       appsv1alpha1.WorkloadSpec{Image: "nginx:latest", UserID: "user-123"},
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
})

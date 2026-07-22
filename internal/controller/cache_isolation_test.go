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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "github.com/gojnimer-labs/ai-cloud-operator/api/v1alpha1"
)

// This exercises the same cache.Options.DefaultNamespaces mechanism
// cmd/main.go's manager is configured with, so two operator instances (each
// scoped to their own WORKLOAD_NAMESPACE) never see each other's Workload
// objects. A cache is built directly here, rather than a full ctrl.Manager,
// since only the cache's namespace-scoping behavior is under test.
var _ = Describe("namespace-scoped cache isolation", func() {
	It("only serves objects from its configured namespace", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		nsA := "cache-iso-a"
		nsB := "cache-iso-b"
		for _, ns := range []string{nsA, nsB} {
			Expect(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			})).To(Succeed())
		}

		workloadA := &appsv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: "wl-a", Namespace: nsA},
		}
		workloadB := &appsv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: "wl-b", Namespace: nsB},
		}
		Expect(k8sClient.Create(ctx, workloadA)).To(Succeed())
		Expect(k8sClient.Create(ctx, workloadB)).To(Succeed())

		cacheA, err := cache.New(cfg, cache.Options{
			Scheme:            scheme.Scheme,
			DefaultNamespaces: map[string]cache.Config{nsA: {}},
		})
		Expect(err).NotTo(HaveOccurred())
		cacheB, err := cache.New(cfg, cache.Options{
			Scheme:            scheme.Scheme,
			DefaultNamespaces: map[string]cache.Config{nsB: {}},
		})
		Expect(err).NotTo(HaveOccurred())

		go func() { _ = cacheA.Start(ctx) }()
		go func() { _ = cacheB.Start(ctx) }()
		Expect(cacheA.WaitForCacheSync(ctx)).To(BeTrue())
		Expect(cacheB.WaitForCacheSync(ctx)).To(BeTrue())

		var listA appsv1alpha1.WorkloadList
		Expect(cacheA.List(ctx, &listA)).To(Succeed())
		Expect(listA.Items).To(HaveLen(1))
		Expect(listA.Items[0].Name).To(Equal("wl-a"))

		var listB appsv1alpha1.WorkloadList
		Expect(cacheB.List(ctx, &listB)).To(Succeed())
		Expect(listB.Items).To(HaveLen(1))
		Expect(listB.Items[0].Name).To(Equal("wl-b"))

		// Reaching across into the other cache's un-scoped namespace errors
		// rather than silently returning nothing — the cache has no informer
		// for that namespace at all.
		err = cacheA.Get(ctx, client.ObjectKeyFromObject(workloadB), &appsv1alpha1.Workload{})
		Expect(err).To(HaveOccurred())
		err = cacheB.Get(ctx, client.ObjectKeyFromObject(workloadA), &appsv1alpha1.Workload{})
		Expect(err).To(HaveOccurred())
	})
})

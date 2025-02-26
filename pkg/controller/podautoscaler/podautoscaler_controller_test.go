/*
Copyright 2024 The Aibrix Team.

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

package podautoscaler

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
)

func IntToPtr(num int32) *int32 {
	return &num
}

var _ = Describe("PodAutoscaler Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		podautoscaler := &autoscalingv1alpha1.PodAutoscaler{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind PodAutoscaler")
			err := k8sClient.Get(ctx, typeNamespacedName, podautoscaler)
			if err != nil && errors.IsNotFound(err) {
				resource := &autoscalingv1alpha1.PodAutoscaler{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: autoscalingv1alpha1.PodAutoscalerSpec{
						ScaleTargetRef: corev1.ObjectReference{
							Kind:      "Service",
							Name:      resourceName,
							Namespace: "default",
						},
						MinReplicas:     IntToPtr(5),
						MaxReplicas:     10,
						ScalingStrategy: "HPA",
						MetricsSources: []autoscalingv1alpha1.MetricSource{
							{
								MetricSourceType: autoscalingv1alpha1.POD,
								ProtocolType:     autoscalingv1alpha1.HTTP,
								TargetMetric:     "test_metric",
								TargetValue:      "1",
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &autoscalingv1alpha1.PodAutoscaler{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance PodAutoscaler")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &PodAutoscalerReconciler{
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
})

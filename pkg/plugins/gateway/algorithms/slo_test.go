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
package routingalgorithms

import (
	"context"
	"encoding/json"
	"os"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func readExampleProfile(filePath string) (*cache.ModelGPUProfile, error) {
	jsonDataBytes, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var profile cache.ModelGPUProfile
	err = json.Unmarshal(jsonDataBytes, &profile)
	if err != nil {
		return nil, err
	}

	return &profile, nil
}

var _ = Describe("SLOQueue", func() {
	var (
		store      *cache.Store
		model      = "llama2-7b"
		deployment = "simulator-llama2-7b-a100"
		router     types.Router
		profileKey = cache.ModelGPUProfileKey(model, deployment)
		profile    *cache.ModelGPUProfile
		err        error
	)

	profile, err = readExampleProfile("../../../../python/aibrix/aibrix/gpu_optimizer/optimizer/profiling/result/simulator-llama2-7b-a100.json")

	BeforeEach(func() {
		Expect(err).To(BeNil())

		store = cache.InitWithInstanceForTest(cache.NewTestCacheWithPods([]*v1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deployment + "-replicaset-pod1",
					Namespace: "default",
					Labels: map[string]string{
						utils.DeploymentIdentifier: deployment,
					},
				},
				Status: v1.PodStatus{
					PodIP: "1.0.0.1",
					Conditions: []v1.PodCondition{
						{
							Type:   v1.PodReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deployment + "-replicaset-pod2",
					Namespace: "default",
					Labels: map[string]string{
						utils.DeploymentIdentifier: deployment,
					},
				},
				Status: v1.PodStatus{
					PodIP: "1.0.0.2",
					Conditions: []v1.PodCondition{
						{
							Type:   v1.PodReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
		}, model))
		store.UpdateModelProfile(profileKey, profile)
		Init() // Required to initialize the router registry
		router, err = NewPackSLORouter(model)
		Expect(err).To(BeNil())
	})

	Describe("Error handling", func() {
		It("Should use fallback router if profile contains no SLO info", func() {
			profile, err := readExampleProfile("../../../../python/aibrix/aibrix/gpu_optimizer/optimizer/profiling/result/simulator-llama2-7b-a100_obsoleted_v1.json")
			Expect(err).To(BeNil())

			store.UpdateModelProfile(profileKey, profile)

			c, _ := cache.Get()
			pods, _ := c.ListPodsByModel(model)

			// Use a context with a timeout to ensure the function doesn't block indefinitely
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			req := RouterSLORouter.NewContext(ctx, model, "message", "request_id")
			_, err = router.Route(req, pods)
			Expect(err).To(BeNil())
			Expect(req.HasRouted()).To(BeTrue())
			Expect(req.Algorithm).To(Equal(RouterLeastRequest))
		})
	})

	// It("Cold start should not throw error", func() {
	// 	c, _ := cache.Get()
	// 	pods, _ := c.ListPodsByModel(model)

	// 	// Use a context with a timeout to ensure the function doesn't block indefinitely
	// 	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	// 	defer cancel()
	// 	req := RouterSLORouter.NewContext(ctx, model, "message", "request_id")
	// 	podAddr, err := router.Route(req, pods)
	// 	Expect(err).To(BeNil())
	// 	Expect(req.HasRouted()).To(BeTrue())
	// 	Expect(req.TargetPod()).ToNot(BeNil())
	// 	Expect([]string{"1.0.0.1", "1.0.0.2"}).To(ContainElement(req.TargetPod().Status.PodIP))
	// 	Expect(podAddr).To(Equal(req.TargetAddress()))
	// })

	// It("Packing router should prefer the same pod", func() {
	// 	c, _ := cache.Get()
	// 	pods, _ := c.ListPodsByModel(model)

	// 	req1 := RouterSLORouter.NewContext(ctx1, model, "message", "request_id")
	// 	req2 := RouterSLORouter.NewContext(context.Background(), model, "message", "request_id")

	// 	podAddr1, err := router.Route(req1, pods)
	// 	Expect(err).To(BeNil())

	// 	pendingLoad, err := c.GetMetricValueByPod(req1.TargetPod().Name, metrics.RealtimeNormalizedPendings)
	// 	Expect(err).To(BeNil())
	// 	Expect(pendingLoad.GetSimpleValue() <= 0.5).To(BeTrue())

	// 	podAddr2, err := router.Route(req2, pods)
	// 	Expect(err).To(BeNil())
	// 	Expect(podAddr1).To(Equal(podAddr2))
	// })
})

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
package cache

import (
	"fmt"
	"math"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

var dummyPod = &v1.Pod{
	ObjectMeta: metav1.ObjectMeta{
		Name: "testpod",
	},
}

func getNewPod(podName string, modelName string, id int) *v1.Pod {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   podName,
			Labels: make(map[string]string),
		},
		Status: v1.PodStatus{
			PodIP: fmt.Sprintf("10.0.0.%d", id),
			Conditions: []v1.PodCondition{
				{
					Type:   v1.PodReady,
					Status: v1.ConditionTrue,
				},
			},
		},
	}
	pod.ObjectMeta.Labels[modelIdentifier] = modelName
	return pod
}

func getNewModelAdapter(modelName string, podName string) *modelv1alpha1.ModelAdapter {
	adapter := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name: modelName,
		},
		Status: modelv1alpha1.ModelAdapterStatus{
			Instances: []string{podName},
		},
	}
	if podName == "" {
		adapter.Status.Instances = nil
	}
	return adapter
}

func getNewModelAdapterWithPods(modelName string, podNames []string) *modelv1alpha1.ModelAdapter {
	adapter := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name: modelName,
		},
		Status: modelv1alpha1.ModelAdapterStatus{
			Instances: podNames,
		},
	}
	return adapter
}

func newTraceCache() *Cache {
	return &Cache{
		initialized:  true,
		requestTrace: &utils.SyncMap[string, *RequestTrace]{},
	}
}

func TestCache(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Cache Suite")
}

func (c *Cache) AddPod(obj interface{}) {
	c.addPod(obj)
}

type lagacyCache struct {
	requestTrace map[string]map[string]int
	mu           sync.RWMutex
}

func (c *lagacyCache) AddRequestTrace(modelName string, inputTokens, outputTokens int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	inputIndex := int64(math.Round(math.Log2(float64(inputTokens)) / RequestTracePrecision)) // Round to the nearest precision and convert to int
	outputIndex := int64(math.Round(math.Log2(float64(outputTokens)) / RequestTracePrecision))

	klog.V(5).Infof("inputTokens: %v, inputIndex: %v, outputTokens: %v, outputIndex: %v",
		inputTokens, inputIndex, outputTokens, outputIndex)

	if len(c.requestTrace[modelName]) == 0 {
		c.requestTrace[modelName] = map[string]int{}
	}

	c.requestTrace[modelName][fmt.Sprintf("%v:%v", inputIndex, outputIndex)] += 1
}

var _ = Describe("Cache", func() {
	It("should both addPod create both pods and metaModels entry", func() {
		cache := newTraceCache()

		// Ignore pods without model label
		podWOModel := getNewPod("p1", "m1", 0)
		podWOModel.ObjectMeta.Labels = nil
		cache.addPod(podWOModel)
		_, exist := cache.metaPods.Load("p1")
		Expect(exist).To(BeFalse())

		// Ignore pods without model label
		podRayWorker := getNewPod("p1", "m1", 0)
		podRayWorker.ObjectMeta.Labels[nodeType] = nodeWorker
		cache.addPod(podRayWorker)
		_, exist = cache.metaPods.Load("p1")
		Expect(exist).To(BeFalse())

		pod := getNewPod("p1", "m1", 0)
		cache.addPod(pod)

		// Pod meta exists
		metaPod, exist := cache.metaPods.Load("p1")
		Expect(exist).To(BeTrue())
		Expect(metaPod.Pod).To(Equal(pod))
		Expect(metaPod.Models).ToNot(BeNil())
		// Pod > model mapping exists.
		modelName, exist := metaPod.Models.Load("m1")
		Expect(exist).To(BeTrue())
		Expect(modelName).To(Equal("m1"))
		// Model meta exists
		metaModel, exist := cache.metaModels.Load("m1")
		Expect(exist).To(BeTrue())
		Expect(metaModel).ToNot(BeNil())
		Expect(metaModel.Pods).ToNot(BeNil())
		// Model -> pod mapping exists
		modelPod, exist := metaModel.Pods.Load("p1")
		Expect(exist).To(BeTrue())
		Expect(modelPod).To(Equal(pod))
		pods := metaModel.Pods.Array()
		Expect(pods.Len()).To(Equal(1))
		Expect(pods.Pods[0]).To(Equal(pod))
		// We'll not test podArray functionality here.
	})

	It("should addModelAdapter create metaModels entry", func() {
		cache := newTraceCache()
		cache.addPod(getNewPod("p1", "m1", 0))

		// Success
		cache.addModelAdapter(getNewModelAdapter("m1adapter", "p1"))

		metaPod, exist := cache.metaPods.Load("p1")
		Expect(exist).To(BeTrue())
		Expect(metaPod.Models.Len()).To(Equal(2))
		// Pod -> adapter mapping exists
		modelName, exist := metaPod.Models.Load("m1adapter")
		Expect(exist).To(BeTrue())
		Expect(modelName).To(Equal("m1adapter"))
		// Model adapter meta exists
		metaModel, exist := cache.metaModels.Load("m1adapter")
		Expect(exist).To(BeTrue())
		Expect(metaModel).ToNot(BeNil())
		Expect(metaModel.Pods).ToNot(BeNil())
		// Model adapter -> pod mapping exists
		modelPod, exist := metaModel.Pods.Load("p1")
		Expect(exist).To(BeTrue())
		Expect(modelPod).To(Equal(metaPod.Pod))
		pods := metaModel.Pods.Array()
		Expect(pods.Len()).To(Equal(1))
		Expect(pods.Pods[0]).To(Equal(metaPod.Pod))

		// Failure
		cache.addModelAdapter(getNewModelAdapter("p0", "m0adapter"))
		// No pod meta automatically created
		_, exist = cache.metaPods.Load("p0")
		Expect(exist).To(BeFalse())
		// No model meta cratead on failure
		_, exist = cache.metaModels.Load("m0adapter")
		Expect(exist).To(BeFalse())
	})

	It("should updatePod clear old mappings with no model adapter inherited", func() {
		cache := newTraceCache()
		oldPod := getNewPod("p1", "m1", 0)
		cache.addPod(oldPod)
		cache.addModelAdapter(getNewModelAdapter("m1adapter", oldPod.Name))
		oldMetaPod, _ := cache.metaPods.Load(oldPod.Name)
		err := cache.updatePodRecord(oldMetaPod, "", metrics.NumRequestsRunning, metrics.PodMetricScope, &metrics.LabelValueMetricValue{Value: "0"})
		Expect(err).To(BeNil())
		Expect(oldMetaPod.Models.Len()).To(Equal(2))
		Expect(oldMetaPod.Metrics.Len()).To(Equal(1))

		newPod := getNewPod("p2", "m1", 1) // IP may changed due to migration

		cache.updatePod(oldPod, newPod)

		// OldPod meta deleted
		_, exist := cache.metaPods.Load("p1")
		Expect(exist).To(BeFalse())
		// NewPod meta created
		newMetaPod, exist := cache.metaPods.Load("p2")
		Expect(exist).To(BeTrue())
		Expect(newMetaPod.Pod).To(Equal(newPod))
		Expect(newMetaPod.Models.Len()).To(Equal(1))
		// Pod -> Model mapping exists
		_, exist = newMetaPod.Models.Load("m1")
		Expect(exist).To(BeTrue())
		// Pod -> Model adapter cleared
		_, exist = newMetaPod.Models.Load("m1adapter")
		Expect(exist).To(BeFalse())
		// Metrics cleared
		Expect(newMetaPod.Metrics.Len()).To(Equal(0))
		// Model meta exists
		metaModel, exist := cache.metaModels.Load("m1")
		Expect(exist).To(BeTrue())
		Expect(metaModel).ToNot(BeNil())
		Expect(metaModel.Pods).ToNot(BeNil())
		Expect(metaModel.Pods.Len()).To(Equal(1))
		// Model -> pod mapping exists
		modelPod, exist := metaModel.Pods.Load("p2")
		Expect(exist).To(BeTrue())
		Expect(modelPod).To(Equal(newPod))
		// Model adapter meta cleared
		_, exist = cache.metaModels.Load("m1adapter")
		Expect(exist).To(BeFalse())
	})

	It("should deletePod clear pod, model, and modelAdapter entrys", func() {
		cache := newTraceCache()
		pod := getNewPod("p1", "m1", 0)
		cache.addPod(pod)
		cache.addModelAdapter(getNewModelAdapter("m1adapter", pod.Name))

		cache.deletePod(pod)

		// Pod meta deleted
		_, exist := cache.metaPods.Load("p1")
		Expect(exist).To(BeFalse())
		// Related model meta deleted
		_, exist = cache.metaModels.Load("m1")
		Expect(exist).To(BeFalse())
		// Related model adapter meta deleted
		_, exist = cache.metaModels.Load("m0adapter")
		Expect(exist).To(BeFalse())

		// Abnormal: Pod without model label exists
		cache.addPod(pod)
		_, exist = cache.metaPods.Load("p1")
		Expect(exist).To(BeTrue())
		pod.ObjectMeta.Labels = nil
		cache.deletePod(pod)
		_, exist = cache.metaPods.Load("p1")
		Expect(exist).To(BeFalse())
	})

	It("should deleteModelAdapter remove mappings", func() {
		cache := newTraceCache()
		cache.addPod(getNewPod("p1", "m1", 0))
		cache.addPod(getNewPod("p2", "m1", 0))
		cache.addPod(getNewPod("p3", "m1", 0))
		oldAdapter := getNewModelAdapterWithPods("m1adapter1", []string{"p1", "p2"})
		cache.addModelAdapter(oldAdapter)

		cache.deleteModelAdapter(oldAdapter)

		// Pod1
		p1MetaPod, exist := cache.metaPods.Load("p1")
		Expect(exist).To(BeTrue())
		Expect(p1MetaPod.Models.Len()).To(Equal(1))
		_, exist = p1MetaPod.Models.Load("m1adapter1")
		Expect(exist).To(BeFalse())

		// Pod2
		p2MetaPod, exist := cache.metaPods.Load("p2")
		Expect(exist).To(BeTrue())
		Expect(p2MetaPod.Models.Len()).To(Equal(1)) // Include base model
		_, exist = p2MetaPod.Models.Load("m1adapter1")
		Expect(exist).To(BeFalse())

		// Pod3
		p3MetaPod, exist := cache.metaPods.Load("p3")
		Expect(exist).To(BeTrue())
		Expect(p3MetaPod.Models.Len()).To(Equal(1)) // Include base model

		// Model intact
		metaModel, exist := cache.metaModels.Load("m1")
		Expect(exist).To(BeTrue())
		Expect(metaModel.Pods.Len()).To(Equal(3))

		// Model adpater1 cleared
		_, exist = cache.metaModels.Load("m1adapter1")
		Expect(exist).To(BeFalse())
	})

	It("should updateModelAdapter reset mappings", func() {
		cache := newTraceCache()
		cache.addPod(getNewPod("p1", "m1", 0))
		cache.addPod(getNewPod("p2", "m1", 0))
		cache.addPod(getNewPod("p3", "m1", 0))
		oldAdapter := getNewModelAdapterWithPods("m1adapter1", []string{"p1", "p2"})
		cache.addModelAdapter(oldAdapter)

		newAdapter := getNewModelAdapterWithPods("m1adapter2", []string{"p2", "p3"})

		cache.updateModelAdapter(oldAdapter, newAdapter)

		// Pod1
		p1MetaPod, exist := cache.metaPods.Load("p1")
		Expect(exist).To(BeTrue())
		Expect(p1MetaPod.Models.Len()).To(Equal(1))
		_, exist = p1MetaPod.Models.Load("m1adapter1")
		Expect(exist).To(BeFalse())
		_, exist = p1MetaPod.Models.Load("m1adapter2")
		Expect(exist).To(BeFalse())

		// Pod2
		p2MetaPod, exist := cache.metaPods.Load("p2")
		Expect(exist).To(BeTrue())
		Expect(p2MetaPod.Models.Len()).To(Equal(2)) // Include base model
		_, exist = p2MetaPod.Models.Load("m1adapter1")
		Expect(exist).To(BeFalse())
		_, exist = p2MetaPod.Models.Load("m1adapter2")
		Expect(exist).To(BeTrue())

		// Pod3
		p3MetaPod, exist := cache.metaPods.Load("p3")
		Expect(exist).To(BeTrue())
		Expect(p3MetaPod.Models.Len()).To(Equal(2)) // Include base model
		_, exist = p3MetaPod.Models.Load("m1adapter1")
		Expect(exist).To(BeFalse())
		_, exist = p3MetaPod.Models.Load("m1adapter2")
		Expect(exist).To(BeTrue())

		// Model adpater1 cleared
		_, exist = cache.metaModels.Load("m1adapter1")
		Expect(exist).To(BeFalse())

		// Model adpater2 registered
		metaModel, exist := cache.metaModels.Load("m1adapter2")
		Expect(exist).To(BeTrue())
		Expect(metaModel).ToNot(BeNil())
		Expect(metaModel.Pods).ToNot(BeNil())
		Expect(metaModel.Pods.Len()).To(Equal(2))
		_, exist = metaModel.Pods.Load("p1")
		Expect(exist).To(BeFalse())
		_, exist = metaModel.Pods.Load("p2")
		Expect(exist).To(BeTrue())
		_, exist = metaModel.Pods.Load("p3")
		Expect(exist).To(BeTrue())
	})

	It("should GetPod return k8s pod", func() {
		cache := newTraceCache()
		pod := getNewPod("p1", "m1", 0)
		cache.addPod(pod)

		_, err := cache.GetPod("p0")
		Expect(err).ToNot(BeNil())

		actual, err := cache.GetPod(pod.Name)
		Expect(err).To(BeNil())
		Expect(actual).To(BeIdenticalTo(pod))
	})

	It("should GetPods return k8s pod slice", func() {
		cache := newTraceCache()
		pod1 := getNewPod("p1", "m1", 0)
		pod2 := getNewPod("p2", "m2", 0)
		cache.addPod(pod1)
		cache.addPod(pod2)

		pods := cache.GetPods()
		Expect(pods).To(HaveLen(2)) // Must be slice
		Expect(pods).To(ContainElement(HaveField("ObjectMeta.Name", "p1")))
		Expect(pods).To(ContainElement(HaveField("ObjectMeta.Name", "p2")))
	})

	It("should GetPodsForModel() return a PodArray", func() {
		cache := newTraceCache()
		pod1 := getNewPod("p1", "m1", 0)
		pod2 := getNewPod("p2", "m2", 0)
		cache.addPod(pod1)
		cache.addPod(pod2)

		_, err := cache.GetPodsForModel("m0")
		Expect(err).ToNot(BeNil())

		pods, err := cache.GetPodsForModel("m1")
		Expect(err).To(BeNil())
		Expect(pods.Len()).To(Equal(1)) // Must be slice
		Expect(pods.Pods).To(HaveLen(1))
		Expect(pods.Pods).To(ContainElement(HaveField("ObjectMeta.Name", "p1")))
	})

	It("should GetModels return string slice", func() {
		cache := newTraceCache()
		pod1 := getNewPod("p1", "m1", 0)
		pod2 := getNewPod("p2", "m2", 0)
		cache.addPod(pod1)
		cache.addPod(pod2)

		exist := cache.CheckModelExists("m0")
		Expect(exist).To(BeFalse())

		exist = cache.CheckModelExists("m1")
		Expect(exist).To(BeTrue())

		models := cache.GetModels()
		Expect(models).To(HaveLen(2)) // Must be slice
		Expect(models).To(ContainElement("m1"))
		Expect(models).To(ContainElement("m2"))
	})

	It("should GetModelsForPod() return a string slice", func() {
		cache := newTraceCache()
		pod1 := getNewPod("p1", "m1", 0)
		pod2 := getNewPod("p2", "m2", 0)
		cache.addPod(pod1)
		cache.addPod(pod2)

		_, err := cache.GetModelsForPod("p0")
		Expect(err).ToNot(BeNil())

		models, err := cache.GetModelsForPod("p1")
		Expect(err).To(BeNil())
		Expect(models).To(HaveLen(1))
		Expect(models).To(ContainElement("m1"))
	})

	It("should basic add request count, add request trace no err", func() {
		modelName := "llama-7b"
		cache := newTraceCache()
		metaPod := cache.addPodLocked(dummyPod)
		cache.addPodAndModelMappingLocked(metaPod, modelName)
		_, exist := cache.metaModels.Load(modelName)
		Expect(exist).To(BeTrue())

		term := cache.AddRequestCount(nil, "no use now", modelName)
		Expect(cache.numRequestsTraces).To(Equal(int32(1)))
		trace := cache.getRequestTrace(modelName)
		Expect(trace).ToNot(BeNil())
		Expect(trace.numKeys).To(Equal(int32(0)))
		Expect(trace.numRequests).To(Equal(int32(1)))
		Expect(trace.completedRequests).To(Equal(int32(0)))
		meta, exist := cache.metaModels.Load(modelName)
		Expect(exist).To(BeTrue())
		Expect(meta.pendingRequests).To(Equal(int32(1)))

		cache.DoneRequestCount(nil, "no use now", modelName, term)
		Expect(cache.numRequestsTraces).To(Equal(int32(1)))
		trace = cache.getRequestTrace(modelName)
		Expect(trace).ToNot(BeNil())
		Expect(trace.numRequests).To(Equal(int32(1)))
		Expect(trace.completedRequests).To(Equal(int32(1)))
		meta, exist = cache.metaModels.Load(modelName)
		Expect(exist).To(BeTrue())
		Expect(meta.pendingRequests).To(Equal(int32(0)))

		cache.DoneRequestTrace(nil, "no use now", modelName, 1, 1, 1)
		Expect(trace.numKeys).To(Equal(int32(1)))
		pProfileCounter, exist := trace.trace.Load("0:0") // log2(1)
		Expect(exist).To(BeTrue())
		Expect(*pProfileCounter.(*int32)).To(Equal(int32(1)))
	})

	It("should global pending counter return 0.", func() {
		modelName := "llama-7b"
		cache := newTraceCache()
		metaPod := cache.addPodLocked(dummyPod)
		cache.addPodAndModelMappingLocked(metaPod, modelName)

		total := 100000
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ { // Repeat N times to increase problem rate
			wg.Add(1)
			// start := time.Now()
			go func() {
				for j := 0; j < total; j++ {
					// Retry until success
					term := cache.AddRequestCount(nil, "no use now", modelName)
					runtime.Gosched()
					cache.DoneRequestTrace(nil, "no use now", modelName, 1, 1, term)
				}
				wg.Done()
			}()
		}
		wg.Wait()
		// duration := time.Since(start)
		// print(duration)
		meta, _ := cache.metaModels.Load(modelName)
		Expect(atomic.LoadInt32(&meta.pendingRequests)).To(Equal(int32(0)))
	})
})

func BenchmarkLagacyAddRequestTrace(b *testing.B) {
	cache := &lagacyCache{
		requestTrace: map[string]map[string]int{},
	}
	thread := 10
	var wg sync.WaitGroup
	for i := 0; i < thread; i++ {
		wg.Add(1)
		go func() {
			for i := 0; i < b.N/thread; i++ {
				cache.AddRequestTrace("model", rand.Int63n(8192), rand.Int63n(1024))
			}
			wg.Done()
		}()
	}
	wg.Wait()
}

func BenchmarkAddRequest(b *testing.B) {
	cache := newTraceCache()
	thread := 10
	var wg sync.WaitGroup
	for i := 0; i < thread; i++ {
		wg.Add(1)
		go func() {
			for i := 0; i < b.N/thread; i++ {
				cache.AddRequestCount(nil, "no use now", "model")
			}
			wg.Done()
		}()
	}
	wg.Wait()
}

func BenchmarkDoneRequest(b *testing.B) {
	cache := newTraceCache()
	thread := 10
	var wg sync.WaitGroup
	term := cache.AddRequestCount(nil, "no use now", "model")
	for i := 0; i < thread; i++ {
		wg.Add(1)
		go func() {
			for i := 0; i < b.N/thread; i++ {
				cache.DoneRequestCount(nil, "no use now", "model", term)
			}
			wg.Done()
		}()
	}
	wg.Wait()
}

func BenchmarkDoneRequestTrace(b *testing.B) {
	cache := newTraceCache()
	thread := 10
	var wg sync.WaitGroup
	term := cache.AddRequestCount(nil, "no use now", "model")
	for i := 0; i < thread; i++ {
		wg.Add(1)
		go func() {
			for i := 0; i < b.N/thread; i++ {
				cache.DoneRequestTrace(nil, "no use now", "model", rand.Int63n(8192), rand.Int63n(1024), term)
			}
			wg.Done()
		}()
	}
	wg.Wait()
}

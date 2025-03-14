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
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

var dummyPod = &Pod{
	Pod: &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "testpod",
		},
	},
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
	It("should basic add request count, add request trace no err", func() {
		modelName := "llama-7b"
		cache := newTraceCache()
		cache.pods.Store(dummyPod.Name, dummyPod)
		cache.addPodAndModelMappingLocked(dummyPod, modelName)
		_, exist := cache.modelMetas.Load(modelName)
		Expect(exist).To(BeTrue())

		term := cache.AddRequestCount(nil, "no use now", modelName)
		Expect(cache.numRequestsTraces).To(Equal(int32(1)))
		trace := cache.getRequestTrace(modelName)
		Expect(trace).ToNot(BeNil())
		Expect(trace.numKeys).To(Equal(int32(0)))
		Expect(trace.numRequests).To(Equal(int32(1)))
		Expect(trace.completedRequests).To(Equal(int32(0)))
		meta, exist := cache.modelMetas.Load(modelName)
		Expect(exist).To(BeTrue())
		Expect(meta.pendingRequests).To(Equal(int32(1)))

		cache.DoneRequestCount(nil, "no use now", modelName, term)
		Expect(cache.numRequestsTraces).To(Equal(int32(1)))
		trace = cache.getRequestTrace(modelName)
		Expect(trace).ToNot(BeNil())
		Expect(trace.numRequests).To(Equal(int32(1)))
		Expect(trace.completedRequests).To(Equal(int32(1)))
		meta, exist = cache.modelMetas.Load(modelName)
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
		cache.pods.Store(dummyPod.Name, dummyPod)
		cache.addPodAndModelMappingLocked(dummyPod, modelName)

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
		meta, _ := cache.modelMetas.Load(modelName)
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

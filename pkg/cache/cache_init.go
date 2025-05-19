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
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
)

var (
	store = &Store{} // Global cache store instance
	once  sync.Once  // Singleton pattern control lock
)

const (
	// For output predictor
	maxInputTokens  = 1024 * 1024       // 1M
	maxOutputTokens = 1024 * 1024       // 1M
	movingWindow    = 240 * time.Second // keep window same with window size of GPU optimizer.
)

// Store contains core data structures and components of the caching system
type Store struct {
	mu                  sync.RWMutex            // Read-write lock for concurrency safety
	initialized         bool                    // Initialization status flag
	redisClient         *redis.Client           // Redis client instance
	prometheusApi       prometheusv1.API        // Prometheus API client
	modelRouterProvider ModelRouterProviderFunc // Function to get model router

	// Metrics related fields
	subscribers         []metrics.MetricSubscriber            // List of metric subscribers
	metrics             map[string]any                        // Generic metric storage
	requestTrace        *utils.SyncMap[string, *RequestTrace] // Request trace data (model_name -> *RequestTrace)
	pendingLoadProvider CappedLoadProvider                    // Provider that defines load in terms of pending requests.
	numRequestsTraces   int32                                 // Request trace counter

	// Pod related storage
	metaPods utils.SyncMap[string, *Pod] // pod_name -> *Pod

	// Model related storage
	metaModels utils.SyncMap[string, *Model] // model_name -> *Model

	// Deploymnent related storage
	deploymentProfiles utils.SyncMap[string, *ModelGPUProfile] // deployment_name -> *ModelGPUProfile

	// buffer for sync map operations
	bufferPod   *Pod
	bufferModel *Model
}

// Get retrieves the cache instance
// Returns:
//
//	Cache: Cache interface instance
//	error: Returns error if cache is not initialized
func Get() (Cache, error) {
	if !store.initialized {
		return nil, errors.New("cache is not initialized")
	}
	return store, nil
}

// New creates a new cache store instance
// Parameters:
//
//	redisClient: Redis client instance
//	prometheusApi: Prometheus API client
//
// Returns:
//
//	Store: Initialized cache store instance
func New(redisClient *redis.Client, prometheusApi prometheusv1.API, modelRouterProvider ModelRouterProviderFunc) *Store {
	return &Store{
		initialized:         true,
		redisClient:         redisClient,
		prometheusApi:       prometheusApi,
		requestTrace:        &utils.SyncMap[string, *RequestTrace]{},
		modelRouterProvider: modelRouterProvider,
	}
}

func NewTestCacheWithPods(pods []*v1.Pod, model string) *Store {
	c := &Store{}
	for _, pod := range pods {
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		pod.Labels[modelIdentifier] = model
		c.addPod(pod)
	}
	return c
}

func NewTestCacheWithPodsMetrics(pods []*v1.Pod, model string, podMetrics map[string]map[string]metrics.MetricValue) *Store {
	c := NewTestCacheWithPods(pods, model)
	c.metaPods.Range(func(podName string, metaPod *Pod) bool {
		if podmetrics, ok := podMetrics[podName]; ok {
			for metricName, metric := range podmetrics {
				if err := c.updatePodRecord(metaPod, model, metricName, metrics.PodMetricScope, metric); err != nil {
					return false
				}
			}
		}
		return true
	})
	return c
}

// InitForTest initializes the cache store for testing purposes, it can be repeated call for reset.
func InitForTest() *Store {
	store = &Store{initialized: true}
	return store
}

// InitWithInstanceForTest initializes the cache store with initialized instance for testing purposes, it can be repeated call for reset.
func InitWithInstanceForTest(st *Store) *Store {
	st.initialized = true
	store = st
	return store
}

// Init initializes the cache store (singleton pattern)
// Parameters:
//
//	config: Kubernetes configuration
//	stopCh: Stop signal channel
//	redisClient: Redis client instance
//
// Returns:
//
//	*Store: Pointer to initialized store instance
func Init(config *rest.Config, stopCh <-chan struct{}) *Store {
	// Configure cache components
	enableGPUOptimizerTracing = false
	enableModelGPUProfileCaching = false
	return InitForGateway(config, stopCh, nil, nil)
}

func InitForMetadata(config *rest.Config, stopCh <-chan struct{}, redisClient *redis.Client) *Store {
	// Configure cache components
	enableGPUOptimizerTracing = false
	enableModelGPUProfileCaching = false
	return InitForGateway(config, stopCh, redisClient, nil)
}

func InitForGateway(config *rest.Config, stopCh <-chan struct{}, redisClient *redis.Client, modelRouterProvider ModelRouterProviderFunc) *Store {
	once.Do(func() {
		store = New(redisClient, initPrometheusAPI(), modelRouterProvider)

		// Initialize cache components
		if err := initCacheInformers(store, config, stopCh); err != nil {
			panic(err)
		}
		initMetricsCache(store, stopCh)
		if enableModelGPUProfileCaching {
			initProfileCache(store, stopCh)
		}
		if enableGPUOptimizerTracing {
			initTraceCache(redisClient, stopCh)
		}
	})

	return store
}

// initMetricsCache initializes metrics cache update loop
// Parameters:
//
//	store: Cache store instance
//	stopCh: Stop signal channel
func initMetricsCache(store *Store, stopCh <-chan struct{}) {
	ticker := time.NewTicker(podMetricRefreshInterval)
	go func() {
		for {
			select {
			case <-ticker.C:
				// Periodically update metrics
				store.updatePodMetrics()
				store.updateModelMetrics()
				if klog.V(5).Enabled() {
					store.debugInfo()
				}
			case <-stopCh:
				ticker.Stop()
				return
			}
		}
	}()
}

// initMetricsCache initializes metrics cache update loop
// Parameters:
//
//	store: Cache store instance
//	stopCh: Stop signal channel
func initProfileCache(store *Store, stopCh <-chan struct{}) {
	store.pendingLoadProvider = newPendingLoadProvider(store)
	ticker := time.NewTicker(defaultModelGPUProfileRefreshInterval)
	go func() {
		for {
			select {
			case <-ticker.C:
				// Periodically update metrics
				store.updateDeploymentProfiles(context.Background())
			case <-stopCh:
				ticker.Stop()
				return
			}
		}
	}()
}

// initTraceCache initializes request tracing cache
// Parameters:
//
//	redisClient: Redis client instance
//	stopCh: Stop signal channel
func initTraceCache(redisClient *redis.Client, stopCh <-chan struct{}) {
	// Calculate time offset for window alignment
	tickerOffset := time.Duration(time.Now().UnixNano()) % RequestTraceWriteInterval
	var traceAlignmentTimer *time.Timer
	var traceTicker *time.Ticker

	// Select alignment method based on offset
	if tickerOffset > MaxRequestTraceIntervalOffset {
		traceAlignmentTimer = time.NewTimer(RequestTraceWriteInterval - tickerOffset)
	} else {
		traceTicker = time.NewTicker(RequestTraceWriteInterval)
	}

	go func() {
		if redisClient == nil {
			return
		}
		if traceAlignmentTimer != nil {
			// Wait for time window alignment
			<-traceAlignmentTimer.C
			traceAlignmentTimer = nil
			traceTicker = time.NewTicker(RequestTraceWriteInterval)
		}
		klog.Infof("trace ticker start at %s", time.Now())
		for {
			select {
			case <-traceTicker.C:
				// Periodically write trace data to storage
				if atomic.LoadInt32(&store.numRequestsTraces) == 0 {
					continue
				}
				t := time.Now().Unix()
				roundT := t - t%int64(RequestTraceWriteInterval/time.Second)
				store.writeRequestTraceToStorage(roundT)
			case <-stopCh:
				traceTicker.Stop()
				return
			}
		}
	}()
}

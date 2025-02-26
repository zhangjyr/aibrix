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
	"fmt"
	"math"
	"math/rand"

	"github.com/vllm-project/aibrix/pkg/cache"
	metrics "github.com/vllm-project/aibrix/pkg/metrics"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type leastKvCacheRouter struct {
	cache *cache.Cache
}

func NewLeastKvCacheRouter() (Router, error) {
	c, err := cache.GetCache()
	if err != nil {
		return nil, err
	}

	return leastKvCacheRouter{
		cache: c,
	}, nil
}

func (r leastKvCacheRouter) Route(ctx context.Context, pods map[string]*v1.Pod, model, message string) (string, error) {
	var targetPodIP string
	minKvCache := math.MaxFloat64

	if len(pods) == 0 {
		return "", fmt.Errorf("no pods to forward request")
	}

	for _, pod := range pods {
		if pod.Status.PodIP == "" {
			continue
		}

		// Due to metric refactor (pull/543) to better support lora and multi models,
		// we change to use PodModelMetrics instead of PodMetrics in some scenarios.
		// This works but doesn't look very promising, we can revisit this part later.
		gpuCache, err := r.cache.GetPodModelMetric(pod.Name, model, metrics.GPUCacheUsagePerc)
		if err != nil {
			klog.Error(err)
			continue
		}
		cpuCache, err := r.cache.GetPodModelMetric(pod.Name, model, metrics.CPUCacheUsagePerc)
		if err != nil {
			klog.Error(err)
			continue
		}
		totalCache := gpuCache.GetSimpleValue() + cpuCache.GetSimpleValue()

		klog.V(4).Infof("pod: %v, podIP: %v, gpuCache: %v, cpuCache: %v, kaCache: %v",
			pod.Name, pod.Status.PodIP, gpuCache.GetSimpleValue(), cpuCache.GetSimpleValue(), totalCache)

		if totalCache <= minKvCache {
			minKvCache = totalCache
			targetPodIP = pod.Status.PodIP
		}
	}

	// Use fallback if no valid metrics
	if targetPodIP == "" {
		klog.Warning("No pods with valid metrics found; selecting a pod randomly as fallback")
		var err error
		targetPodIP, err = selectRandomPod(pods, rand.Intn)
		if err != nil {
			return "", err
		}
	}

	if targetPodIP == "" {
		return "", fmt.Errorf("no pods to forward request")
	}

	klog.V(4).Infof("targetPodIP: %v", targetPodIP)
	return targetPodIP + ":" + podMetricPort, nil
}

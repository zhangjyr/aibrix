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
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type leastRequestRouter struct {
	cache *cache.Cache
}

func NewLeastRequestRouter() (Router, error) {
	c, err := cache.GetCache()
	if err != nil {
		return nil, err
	}

	return leastRequestRouter{
		cache: c,
	}, nil
}

func (r leastRequestRouter) Route(ctx context.Context, pods map[string]*v1.Pod, model, message string) (string, error) {
	var targetPodIP string
	minCount := math.MaxFloat64

	if len(pods) == 0 {
		return "", fmt.Errorf("no pods to forward request")
	}

	readyPods := utils.FilterReadyPods(pods)
	if len(readyPods) == 0 {
		return "", fmt.Errorf("no ready pods available for fallback")
	}

	for _, pod := range readyPods {
		runningReq, err := r.cache.GetPodModelMetric(pod.Name, model, metrics.NumRequestsRunning)
		if err != nil {
			klog.Error(err)
			continue
		}
		waitingReq, err := r.cache.GetPodModelMetric(pod.Name, model, metrics.NumRequestsWaiting)
		if err != nil {
			klog.Error(err)
			continue
		}
		swappedReq, err := r.cache.GetPodModelMetric(pod.Name, model, metrics.NumRequestsSwapped)
		if err != nil {
			klog.Error(err)
			continue
		}

		totalReq := runningReq.GetSimpleValue() + waitingReq.GetSimpleValue() + swappedReq.GetSimpleValue()
		klog.V(4).Infof("pod: %v, podIP: %v, runningReq: %v, waitingReq: %v, swappedReq: %v, totalReq: %v",
			pod.Name, pod.Status.PodIP, runningReq, waitingReq, swappedReq, totalReq)

		if totalReq <= minCount {
			minCount = totalReq
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

	return targetPodIP + ":" + podMetricPort, nil
}

func (r *leastRequestRouter) SubscribedMetrics() []string {
	return []string{
		metrics.NumRequestsRunning,
		metrics.NumRequestsWaiting,
		metrics.NumRequestsSwapped,
	}
}

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
	"fmt"
	"math"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

var (
	ErrorNoAvailablePod = fmt.Errorf("no pod available")
)

type leastLoadRouter struct {
	provider       cache.LoadProvider
	cappedProvider cache.CappedLoadProvider
	pulling        bool
}

func NewLeastLoadRouter(provider cache.LoadProvider) (types.Router, error) {
	return &leastLoadRouter{
		provider: provider,
	}, nil
}

func NewLeastLoadPullingRouter(provider cache.CappedLoadProvider) (types.Router, error) {
	return &leastLoadRouter{
		provider:       provider,
		cappedProvider: provider,
		pulling:        true,
	}, nil
}

func (r *leastLoadRouter) Route(ctx *types.RoutingContext, pods *utils.PodArray) (string, error) {
	if len(pods.Pods) == 0 {
		return "", fmt.Errorf("no pods to forward request")
	}

	var targetPod *v1.Pod
	minUtil := math.MaxFloat64
	if r.pulling {
		minUtil = r.cappedProvider.Cap()
	}

	for _, pod := range pods.Pods {
		if pod.Status.PodIP == "" {
			// Pod not ready.
			continue
		}

		util, err := r.provider.GetUtilization(ctx, pod)
		if err != nil {
			klog.Error(err)
			continue
		}

		klog.V(4).Infof("pod: %v, podIP: %v, util: %.2f",
			pod.Name, pod.Status.PodIP, util)

		var consumption float64
		if r.pulling {
			consumption, err = r.provider.GetConsumption(ctx, pod)
			if err != nil {
				klog.Error(err)
				continue
			}
		}

		util += consumption // 0 if not in pulling mode
		if util <= minUtil {
			minUtil = util
			targetPod = pod
		}
	}

	// No fallback
	if targetPod == nil {
		return "", ErrorNoAvailablePod
	}

	klog.V(4).Infof("targetPod: %s(%s)", targetPod.Name, targetPod.Status.PodIP)
	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

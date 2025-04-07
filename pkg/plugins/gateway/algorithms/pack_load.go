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

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type packLoadRouter struct {
	provider cache.CappedLoadProvider
}

func NewPackLoadRouter(provider cache.CappedLoadProvider) (types.Router, error) {
	return &packLoadRouter{
		provider: provider,
	}, nil
}

func (r *packLoadRouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	klog.V(4).Info("Routing using packLoadRouter", "candidates", pods.Len())

	if pods.Len() == 0 {
		return "", fmt.Errorf("no pods to forward request")
	}

	var targetPod *v1.Pod
	capUtil := r.provider.Cap()
	maxUtil := 0.0

	for _, pod := range pods.All() {
		if pod.Status.PodIP == "" {
			// Pod not ready.
			klog.V(4).InfoS("Skipped pod due to missing PodIP in packLoadRouter", "pod", pod.Name, "PodIP", pod.Status.PodIP)
			continue
		}

		util, err := r.provider.GetUtilization(ctx, pod)
		if err != nil {
			klog.V(4).InfoS("Skipped pod due to fail to get utilization in packLoadRouter", "pod", pod.Name, "error", err)
			continue
		}

		consumption, err := r.provider.GetConsumption(ctx, pod)
		if err != nil {
			klog.V(4).InfoS("Skipped pod due to fail to get consumption in packLoadRouter", "pod", pod.Name, "error", err)
			continue
		}

		klog.V(4).Infof("pod: %v, podIP: %v, consumption: %.2f, util: %.2f", pod.Name, pod.Status.PodIP, consumption, util)

		util += consumption
		if util > maxUtil && util <= capUtil {
			maxUtil = util
			targetPod = pod
		}
	}

	// Use fallback if no valid metrics
	if targetPod == nil {
		return "", ErrorNoAvailablePod
	}

	klog.V(4).Infof("targetPod: %s(%s)", targetPod.Name, targetPod.Status.PodIP)
	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

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

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
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

func (r *packLoadRouter) Route(ctx context.Context, pods *utils.PodArray, req *types.RouterRequest) (string, error) {
	if len(pods.Pods) == 0 {
		return "", fmt.Errorf("no pods to forward request")
	}

	var targetPod *v1.Pod
	capUtil := r.provider.Cap()
	maxUtil := 0.0

	for _, pod := range pods.Pods {
		if pod.Status.PodIP == "" {
			// Pod not ready.
			continue
		}

		util, err := r.provider.GetUtilization(ctx, pod, req.Model)
		if err != nil {
			klog.Error(err)
			continue
		}

		klog.V(4).Infof("pod: %v, podIP: %v, util: %.2f",
			pod.Name, pod.Status.PodIP, util)

		consumption, err := r.provider.GetConsumption(ctx, pod, req)
		if err != nil {
			klog.Error(err)
			continue
		}

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
	req.SetTargetPod(targetPod)
	return req.TargetAddress(), nil
}

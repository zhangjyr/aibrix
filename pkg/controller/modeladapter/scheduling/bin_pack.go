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

package scheduling

import (
	"context"
	"math"

	"github.com/vllm-project/aibrix/pkg/cache"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type binPackScheduler struct {
	cache *cache.Cache
}

func NewBinPackScheduler(c *cache.Cache) Scheduler {
	return binPackScheduler{
		cache: c,
	}
}

func (r binPackScheduler) SelectPod(ctx context.Context, model string, pods []v1.Pod) (*v1.Pod, error) {
	// Binpack algorithm: choose the pod (1) can place the adapter, (2) with the least remaining space

	selectedPod := v1.Pod{}
	podRemainCapMin := math.MaxInt

	for _, pod := range pods {
		models, err := r.cache.GetModelsForPod(pod.Name)
		if err != nil {
			return nil, err
		}
		podCap := 10 // todo: replace mock data
		if len(models) >= podCap {
			continue
		}

		if podCap-len(models) < podRemainCapMin {
			selectedPod = pod
			podRemainCapMin = podCap - len(models)
		}
	}

	klog.InfoS("pod selected with first fit", "pod", klog.KObj(&selectedPod))
	return &selectedPod, nil
}

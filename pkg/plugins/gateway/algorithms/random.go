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
	"math/rand"

	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

var (
	RouterRandom Algorithms = "random"
)

func init() {
	Register(RouterRandom, func(*types.RoutingContext) (types.Router, error) { return NewRandomRouter() })
}

type randomRouter struct {
}

func NewRandomRouter() (types.Router, error) {
	return randomRouter{}, nil
}

func (r randomRouter) Route(ctx *types.RoutingContext, pods *utils.PodArray) (string, error) {
	var targetPod *v1.Pod
	if len(pods.Pods) == 0 {
		return "", fmt.Errorf("no pods to forward request")
	}

	var err error
	targetPod, err = selectRandomPod(pods.Pods, rand.Intn)
	if err != nil {
		return "", err
	}

	if targetPod == nil {
		return "", fmt.Errorf("no pods to forward request")
	}

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

func (r *randomRouter) SubscribedMetrics() []string {
	return []string{}
}

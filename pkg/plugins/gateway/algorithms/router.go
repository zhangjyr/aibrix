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
	"github.com/vllm-project/aibrix/pkg/types"
	"k8s.io/klog/v2"
)

const (
	RouterNotSet = ""
)

// Validate validates if user provided routing routers is supported by gateway
func Validate(algorithms string) (types.RoutingAlgorithms, bool) {
	if _, ok := routerRegistry[types.RoutingAlgorithms(algorithms)]; ok {
		return types.RoutingAlgorithms(algorithms), ok
	} else {
		return RouterNotSet, false
	}
}

// Select the user provided router provider supported by gateway, no error reported and fallback to random router
// Call Validate before this function to ensure expected behavior.
func Select(algorithms types.RoutingAlgorithms) types.RouterProviderFunc {
	if provider, ok := routerRegistry[algorithms]; ok {
		return provider
	} else {
		klog.Warningf("Unsupported router strategy: %s, use %s instead.", algorithms, RouterRandom)
		return routerRegistry[RouterRandom]
	}
}

func Register(algorithms types.RoutingAlgorithms, router types.RouterProviderFunc) {
	routerRegistry[algorithms] = router
}

var routerRegistry = map[types.RoutingAlgorithms]types.RouterProviderFunc{}

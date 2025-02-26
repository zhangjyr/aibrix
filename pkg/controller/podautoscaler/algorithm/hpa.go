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

package algorithm

import "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/common"

// HpaScalingAlgorithm can be used by any scaler without customized algorithms
type HpaScalingAlgorithm struct{}

var _ ScalingAlgorithm = (*HpaScalingAlgorithm)(nil)

func (a *HpaScalingAlgorithm) ComputeTargetReplicas(currentPodCount float64, context common.ScalingContext) int32 {
	// TODO: implement me!
	return int32(currentPodCount)
}

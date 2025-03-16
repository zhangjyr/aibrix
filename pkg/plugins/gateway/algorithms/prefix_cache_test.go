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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_PrefixCache(t *testing.T) {
	prefixCacheRouter, _ := NewPrefixCacheRouter()

	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "p1"},
			Status: v1.PodStatus{
				PodIP: "1.1.1.1",
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			}},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "p2"},
			Status: v1.PodStatus{
				PodIP: "2.2.2.2",
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			}},
	}

	targetPod, err := prefixCacheRouter.Route(types.NewRoutingContext(context.Background(), "m1", "this is first message"), &utils.PodArray{Pods: pods})
	assert.NoError(t, err)

	targetPod2, err := prefixCacheRouter.Route(types.NewRoutingContext(context.Background(), "m1", "this is first message"), &utils.PodArray{Pods: pods})
	assert.NoError(t, err)

	assert.Equal(t, targetPod, targetPod2)
}

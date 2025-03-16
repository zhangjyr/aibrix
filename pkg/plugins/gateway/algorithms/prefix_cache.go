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
	"strconv"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

var (
	RouterPrefixCache Algorithms = "prefix-cache"
)

func init() {
	Register(RouterPrefixCache, func(*types.RoutingContext) (types.Router, error) { return NewPrefixCacheRouter() })
}

const (
	defaultPrefixCacheMatchThresholdPercent = 50
)

var (
	prefixCacheMatchThresholdPercent = getPrefixCacheMatchThresholdPercent()
)

func getPrefixCacheMatchThresholdPercent() int {
	value := utils.LoadEnv("AIBRIX_PREFIX_CACHE_MATCH_THRESHOLD_PERCENT", "")
	if value != "" {
		intValue, err := strconv.Atoi(value)
		if err != nil || intValue <= 0 || intValue > 100 {
			klog.Infof("invalid AIBRIX_PREFIX_CACHE_MATCH_THRESHOLD_PERCENT: %s, valid value between 0 and 100, failing back to default", value)
		} else {
			klog.Infof("using AIBRIX_PREFIX_CACHE_MATCH_THRESHOLD_PERCENT env value for prefix cache match threshold percent: %d", intValue)
			return intValue
		}
	}
	klog.Infof("using default prefix cache match threshold percent: %d", defaultPrefixCacheMatchThresholdPercent)
	return defaultPrefixCacheMatchThresholdPercent
}

type prefixCacheRouter struct {
	prefixCacheIndexer prefixcacheindexer.PrefixCacheIndexer
}

func NewPrefixCacheRouter() (types.Router, error) {
	return prefixCacheRouter{
		prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable(),
	}, nil
}

func (p prefixCacheRouter) Route(ctx *types.RoutingContext, pods *utils.PodArray) (string, error) {
	readyPods := utils.FilterRoutablePods(pods.Pods)
	if len(readyPods) == 0 {
		return "", fmt.Errorf("no pods to forward request")
	}
	if len(readyPods) == 1 {
		for _, pod := range pods.Pods {
			ctx.SetTargetPod(pod)
			return ctx.TargetAddress(), nil
		}
	}

	tokens, err := utils.TokenizeInputText(ctx.Message)
	if err != nil {
		return "", err
	}

	var targetPod *v1.Pod
	matchedTokens, unMatchedTokens, matchedPods := p.prefixCacheIndexer.MatchPrefix(tokens, ctx.Model, readyPods)
	if len(matchedTokens)*100/len(tokens) > prefixCacheMatchThresholdPercent {
		targetPod = matchedPods[rand.Intn(len(matchedPods))]
	} else {
		// TODO: add better load balanced algorithms as fallback
		targetPod = readyPods[rand.Intn(len(readyPods))]
	}
	if len(unMatchedTokens) > 0 {
		p.prefixCacheIndexer.AddPrefix(unMatchedTokens, ctx.Model, targetPod.Name)
	}

	var matchedPodNames, readyPodNames []string
	for _, p := range matchedPods {
		matchedPodNames = append(matchedPodNames, p.Status.PodIP)
	}
	for _, p := range readyPods {
		readyPodNames = append(readyPodNames, p.Status.PodIP)
	}
	klog.InfoS("prefix cache route",
		"matched_tokens", matchedTokens,
		"unmatched_tokens", unMatchedTokens,
		"matched_pods", matchedPodNames,
		"ready_pods", readyPodNames,
		"target_pod", targetPod.Status.PodIP)

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

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

package types

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

const podMetricPort = "8000"

type RequestFeatures []float64

// RoutingAlgorithms defines the routing algorithms
type RoutingAlgorithms string

// RoutingContext encapsulates the context information required for routing.
// It can be extended with more fields as needed in the future.
type RoutingContext struct {
	context.Context
	Algorithm   RoutingAlgorithms
	Model       string
	Message     string
	RequestTime time.Time
	PendingLoad float64

	chTargetPod chan *v1.Pod
	targetPod   *v1.Pod
	tokens      []int
	predictor   OutputPredictor
}

var requestPool = sync.Pool{
	New: func() any {
		return &RoutingContext{
			chTargetPod: make(chan *v1.Pod),
		}
	},
}

func (alg RoutingAlgorithms) NewContext(ctx context.Context, model string, message string, predictor OutputPredictor) *RoutingContext {
	request := requestPool.Get().(*RoutingContext)
	request.reset(ctx, alg, model, message, predictor)
	return request
}

func NewRoutingContext(ctx context.Context, algorithms RoutingAlgorithms, model string, message string, predictor OutputPredictor) *RoutingContext {
	request := requestPool.Get().(*RoutingContext)
	request.reset(ctx, algorithms, model, message, predictor)
	return request
}

func (r *RoutingContext) Delete() {
	requestPool.Put(r)
}

func (r *RoutingContext) PromptTokens() ([]int, error) {
	if r.tokens == nil {
		var err error
		r.tokens, err = utils.TokenizeInputText(r.Message)
		if err != nil {
			return nil, err
		}
	}
	return r.tokens, nil
}

func (r *RoutingContext) PromptLength() (int, error) {
	tokens, err := r.PromptTokens()
	if err != nil {
		return 0, err
	}
	return len(tokens), nil
}

func (r *RoutingContext) TokenLength() (int, error) {
	promptLen, err := r.PromptLength()
	if err != nil {
		return 0, err
	}

	if r.predictor == nil {
		return 0, fmt.Errorf("output predictor not set")
	}

	return r.predictor.Predict(promptLen), nil
}

func (r *RoutingContext) Features() (RequestFeatures, error) {
	promptLen, err := r.PromptLength()
	if err != nil {
		return nil, err
	}

	outputLen, err := r.TokenLength()
	if err != nil {
		return nil, err
	}

	return RequestFeatures{float64(outputLen), float64(promptLen)}, nil
}

func (r *RoutingContext) SetTargetPod(pod *v1.Pod) {
	r.targetPod = pod
	// Notify possible waiting PodIP() call, abundon if no waiting.
	select {
	case r.chTargetPod <- pod:
	default:
	}
}

func (r *RoutingContext) TargetPod() *v1.Pod {
	if r.targetPod == nil {
		r.targetPod = <-r.chTargetPod
		// Notify next waiting PodIP() call, abundon if no waiting.
		r.SetTargetPod(r.targetPod)
	}

	return r.targetPod
}

func (r *RoutingContext) TargetAddress() string {
	return r.targetAddress(r.TargetPod())
}

func (r *RoutingContext) HasRouted() bool {
	return r.targetPod != nil
}

func (r *RoutingContext) targetAddress(pod *v1.Pod) string {
	return fmt.Sprintf("%v:%v", pod.Status.PodIP, podMetricPort)
}

func (r *RoutingContext) reset(ctx context.Context, algorithms RoutingAlgorithms, model string, message string, predictor OutputPredictor) {
	r.Context = ctx
	r.Algorithm = algorithms
	r.Model = model
	r.Message = message
	r.tokens = nil
	r.predictor = predictor
}

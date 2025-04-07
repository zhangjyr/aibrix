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
	"sync/atomic"
	"time"

	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

const podMetricPort = "8000"

var nilPod = &v1.Pod{}

type RequestFeatures []float64

// RoutingAlgorithms defines the routing algorithms
type RoutingAlgorithm string

// RoutingContext encapsulates the context information required for routing.
// It can be extended with more fields as needed in the future.
type RoutingContext struct {
	context.Context
	Algorithm   RoutingAlgorithm
	RequestID   string
	Model       string
	Message     string
	RequestTime time.Time // Time when the routing context is created.
	PendingLoad float64   // Normalized pending load of request, available after AddRequestCount call.
	TraceTerm   int64     // Trace term identifier, available after AddRequestCount call.
	RoutedTime  time.Time

	targetPodSet chan struct{}
	targetPod    atomic.Pointer[v1.Pod]
	lastError    error
	debugDelay   time.Duration
	tokens       []int
	predictor    OutputPredictor
	statsUpdated int32 // Use to flag if in-memory realtime statistics has been updated for the request.
}

var requestPool = sync.Pool{
	New: func() any { return &RoutingContext{} },
}

func (alg RoutingAlgorithm) NewContext(ctx context.Context, requestID, model, message string) *RoutingContext {
	request := requestPool.Get().(*RoutingContext)
	request.reset(ctx, alg, requestID, model, message)
	return request
}

func NewRoutingContext(ctx context.Context, algorithm RoutingAlgorithm, requestID, model, message string) *RoutingContext {
	request := requestPool.Get().(*RoutingContext)
	request.reset(ctx, algorithm, requestID, model, message)
	return request
}

func (r *RoutingContext) SetOutputPreditor(predictor OutputPredictor) (old OutputPredictor) {
	old = r.predictor
	r.predictor = predictor
	return
}

func (r *RoutingContext) Delete() {
	r.SetTargetPod(nil) // Unblock waiting TargetPod() call
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
	if r.targetPod.CompareAndSwap(nilPod, pod) { // Use CompareAndSwap to ensure close channel only once
		r.RoutedTime = time.Now()
		close(r.targetPodSet)
	}
}

func (r *RoutingContext) SetError(err error) {
	r.lastError = err
	r.SetTargetPod(nil)
}

func (r *RoutingContext) TargetPod() *v1.Pod {
	targetPod := r.targetPod.Load()
	if targetPod == nilPod {
		r.debugWait()
		<-r.targetPodSet // No blocking if targetPod is set after last "targetPod == nil"
		targetPod = r.targetPod.Load()
	}

	return targetPod
}

func (r *RoutingContext) GetError() error {
	if r.TargetPod() == nil {
		return r.lastError
	}
	return nil
}

func (r *RoutingContext) TargetAddress() string {
	pod := r.TargetPod()
	if pod == nil {
		return ""
	}
	return r.targetAddress(r.TargetPod())
}

func (r *RoutingContext) HasRouted() bool {
	pod := r.targetPod.Load()
	return pod != nilPod && pod != nil
}

func (r *RoutingContext) CanUpdateStats() bool {
	return atomic.CompareAndSwapInt32(&r.statsUpdated, 0, 1)
}

func (r *RoutingContext) GetRoutingDelay() time.Duration {
	return r.RoutedTime.Sub(r.RequestTime)
}

func (r *RoutingContext) targetAddress(pod *v1.Pod) string {
	return fmt.Sprintf("%v:%v", pod.Status.PodIP, podMetricPort)
}

func (r *RoutingContext) reset(ctx context.Context, algorithms RoutingAlgorithm, requestID string, model string, message string) {
	r.Context = ctx
	r.Algorithm = algorithms
	r.RequestID = requestID
	r.Model = model
	r.Message = message
	r.RequestTime = time.Now()
	r.tokens = nil
	r.predictor = nil
	r.targetPodSet = make(chan struct{}) // Initialize channel
	r.targetPod.Store(nilPod)
	r.lastError = nil
}

func (r *RoutingContext) debugWait() {
	if r.debugDelay > 0 {
		time.Sleep(r.debugDelay)
	}
}

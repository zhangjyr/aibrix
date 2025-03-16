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
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

const (
	RouterQueue     Algorithms = "queue"
	QueueBufferSize            = 1024
)

func init() {
	Register(RouterQueue, func(req *types.RoutingContext) (types.Router, error) {
		c, err := cache.GetCache()
		if err != nil {
			return nil, err
		}
		return c.GetQueueRouter(req.Model)
	})
}

type queueRouter struct {
	router         types.Router
	queue          utils.GlobalQueue[*types.RoutingContext]
	chRouteTrigger chan *utils.PodArray

	// providers             []*Producer
	// dequeueCandidates     []*CandidateCustomer
	// lastSubCandidate      int
	// lastConsumerCandidate *ConsumerQueue
	// lastPeek              float64

	// // Feature switch
	// peekDelay             float64
	// queueOverallSLO       bool
	// monogenousGPURouting  bool
	// useProfileServiceTime bool

	// provider CappedLoadProvider
}

func newQueueRouter(backend types.Router, queue utils.GlobalQueue[*types.RoutingContext]) (types.Router, error) {
	router := &queueRouter{
		router:         backend,
		queue:          queue,
		chRouteTrigger: make(chan *utils.PodArray),
	}

	go router.serve()

	return router, nil
}

func NewQueueRouter(modelName string) (types.Router, error) {
	router, err := NewRandomRouter()
	if err != nil {
		return nil, err
	}
	queue := utils.NewSimpleQueue[*types.RoutingContext](QueueBufferSize)
	return newQueueRouter(router, queue)
}

func (r *queueRouter) Route(ctx *types.RoutingContext, pods *utils.PodArray) (string, error) {
	if len(pods.Pods) == 0 {
		return "", fmt.Errorf("no pods to forward request")
	}

	if ctx == nil {
		r.tryRoute(pods) // Simply trigger a possible dequeue
		return "", nil   // Result is irrelevant
	}

	now := time.Now()
	ctx.RequestTime = now
	r.queue.Enqueue(ctx, now)

	r.tryRoute(pods) // Simply trigger a possible dequeue

	targetPod := ctx.TargetPod() // Will wait

	klog.V(4).Infof("targetPod: %s(%s)", targetPod.Name, targetPod.Status.PodIP)
	return ctx.TargetAddress(), nil
}

func (r *queueRouter) tryRoute(pods *utils.PodArray) {
	select {
	case r.chRouteTrigger <- pods:
	default:
		// ignore
	}
}

func (r *queueRouter) serve() {
	for {
		pods := <-r.chRouteTrigger

		for {
			ctx, err := r.queue.Peek(time.Now(), pods)
			if ctx == nil || err != nil {
				klog.Errorf("error on peek request queue: %v", err)
				break
			}

			_, err = r.router.Route(ctx, pods)
			// Ignore err
			if err == nil {
				// req.SetTargetPod() should have called in Route()
				dequeued, err := r.queue.Dequeue()
				if err != nil {
					klog.Errorf("error on dequeue request queue: %v", err)
				} else if dequeued != ctx {
					klog.Error("unexpected request dequeued")
				}
			}
		}
	}
}

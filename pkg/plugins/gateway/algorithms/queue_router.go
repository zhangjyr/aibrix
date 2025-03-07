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
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

var (
	RouterQueueRouter Algorithms = "queue"
)

func init() {
	Register(RouterQueueRouter, func(req *types.RouterRequest) (types.Router, error) {
		c, err := cache.GetCache()
		if err != nil {
			return nil, err
		}
		return c.GetQueueRouter(req.Model)
	})
}

type queueRouter struct {
	router         types.Router
	queue          utils.GlobalQueue[*types.RouterRequest]
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

func NewQueueRouter(backend types.Router, queue utils.GlobalQueue[*types.RouterRequest]) (types.Router, error) {
	router := &queueRouter{
		router:         backend,
		queue:          queue,
		chRouteTrigger: make(chan *utils.PodArray),
	}

	go router.serve()

	return router, nil
}

func NewPackSLORouter(modelName string) (types.Router, error) {
	loadProvider, err := cache.NewPendingLoadProvider()
	if err != nil {
		return nil, err
	}

	loadRouter, _ := NewPackLoadRouter(loadProvider)
	sloQueue := NewSLOQueue(loadRouter, modelName)
	return NewQueueRouter(sloQueue, sloQueue)
}

func NewLeastLoadSLORouter(modelName string) (types.Router, error) {
	loadProvider, err := cache.NewPendingLoadProvider()
	if err != nil {
		return nil, err
	}

	loadRouter, _ := NewLeastLoadPullingRouter(loadProvider)
	sloQueue := NewSLOQueue(loadRouter, modelName)
	return NewQueueRouter(sloQueue, sloQueue)
}

func (r *queueRouter) Route(ctx context.Context, pods *utils.PodArray, req *types.RouterRequest) (string, error) {
	if len(pods.Pods) == 0 {
		return "", fmt.Errorf("no pods to forward request")
	}

	if req == nil {
		r.tryRoute(pods) // Simply trigger a possible dequeue
		return "", nil   // Result is irrelevant
	}

	now := time.Now()
	req.Context = ctx
	req.RequestTime = now
	r.queue.Enqueue(req, now)

	r.tryRoute(pods) // Simply trigger a possible dequeue

	targetPod := req.TargetPod() // Will wait

	klog.V(4).Infof("targetPod: %s(%s)", targetPod.Name, targetPod.Status.PodIP)
	return req.TargetAddress(), nil
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
			req, err := r.queue.Peek(time.Now(), pods)
			if req == nil || err != nil {
				klog.Errorf("error on peek request queue: %v", err)
				break
			}

			_, err = r.router.Route(req.Context, pods, req)
			// Ignore err
			if err == nil {
				// req.SetTargetPod() should have called in Route()
				dequeued, err := r.queue.Dequeue()
				if err != nil {
					klog.Errorf("error on dequeue request queue: %v", err)
				} else if dequeued != req {
					klog.Error("unexpected request dequeued")
				}
			}
		}
	}
}

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
	"k8s.io/klog/v2"
)

var (
	RouterSLORouter types.RoutingAlgorithms = "slo-router"
)

func init() {
	Register(RouterSLORouter, func(ctx *types.RoutingContext) (types.Router, error) {
		c, err := cache.Get()
		if err != nil {
			return nil, err
		}
		return c.GetRouter(ctx)
	})
}

type queueRouter struct {
	router         types.Router
	queue          types.GlobalQueue[*types.RoutingContext]
	chRouteTrigger chan types.PodList
}

func NewQueueRouter(backend types.Router, queue types.GlobalQueue[*types.RoutingContext]) (types.Router, error) {
	router := &queueRouter{
		router:         backend,
		queue:          queue,
		chRouteTrigger: make(chan types.PodList),
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

func (r *queueRouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	if pods.Len() == 0 {
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

func (r *queueRouter) tryRoute(pods types.PodList) {
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

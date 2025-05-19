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
package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

const (
	expireWriteRequestTraceIntervalInMins = 10
	traceLogInterval                      = 1 * time.Second
)

func (c *Store) getRequestTrace(modelName string) *RequestTrace {
	trace := NewRequestTrace(time.Now().UnixNano())
	newer, loaded := c.requestTrace.LoadOrStore(modelName, trace)
	if loaded {
		trace.Recycle()
	} else {
		atomic.AddInt32(&c.numRequestsTraces, 1)
	}
	return newer
}

func (c *Store) addPodStats(ctx *types.RoutingContext, requestID string) {
	if !ctx.HasRouted() {
		return
	}
	pod := ctx.TargetPod()
	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	metaPod, ok := c.metaPods.Load(key)
	if !ok {
		klog.Warningf("can't find routing pod: %s, requestID: %s", pod.Name, requestID)
		return
	}

	// Update running requests
	requests := atomic.AddInt32(&metaPod.runningRequests, 1)
	if err := c.updatePodRecord(metaPod, "", metrics.RealtimeNumRequestsRunning, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: float64(requests)}); err != nil {
		klog.Warningf("can't update realtime metric: %s, pod: %s, requestID: %s, err: %v", metrics.RealtimeNumRequestsRunning, metaPod.Name, requestID, err)
	}

	// Update pending load
	var utilization float64
	if c.pendingLoadProvider != nil {
		var err error
		ctx.PendingLoad, err = c.pendingLoadProvider.GetConsumption(ctx, pod)
		if err != nil {
			klog.Errorf("error on track request load consumption: %v", err)
		} else {
			utilization = metaPod.pendingLoadUtilization.Add(ctx.PendingLoad)
			if c.updatePodRecord(metaPod, "", metrics.RealtimeNormalizedPendings, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: utilization}) != nil {
				klog.Warningf("can't update realtime metric: %s, pod: %s, requestID: %s, err: %v", metrics.RealtimeNormalizedPendings, metaPod.Name, requestID, err)
			}
		}
	}

	if metaPod.CanLogPodTrace() {
		klog.V(5).InfoS("pod stats updated.", "pod", metaPod.Name, "running_requests", requests, "pending_load", utilization)
	}
}

func (c *Store) donePodStats(ctx *types.RoutingContext, requestID string) {
	if !ctx.HasRouted() {
		return
	}
	pod := ctx.TargetPod()

	// Now that pendingLoadProvider must be set.
	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	metaPod, ok := c.metaPods.Load(key)
	if !ok {
		klog.Warningf("can't find routing pod: %s, requestID: %s", pod.Name, requestID)
		return
	}

	// Update running requests
	requests := atomic.AddInt32(&metaPod.runningRequests, -1)
	if err := c.updatePodRecord(metaPod, ctx.Model, metrics.RealtimeNumRequestsRunning, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: float64(requests)}); err != nil {
		klog.Warningf("can't update realtime metric: %s, pod: %s, requestID: %s", metrics.RealtimeNumRequestsRunning, pod.Name, requestID)
	}

	// Update pending load
	var utilization float64
	if ctx.PendingLoad != 0.0 {
		utilization := metaPod.pendingLoadUtilization.Add(-ctx.PendingLoad)
		if c.updatePodRecord(metaPod, ctx.Model, metrics.RealtimeNormalizedPendings, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: utilization}) != nil {
			klog.Warningf("can't update realtime metric: %s, pod: %s, requestID: %s", metrics.RealtimeNormalizedPendings, pod.Name, requestID)
		}
		if utilization < c.pendingLoadProvider.Cap() {
			// Notify queue router to try route with pending requests.
			if metaModel, ok := c.metaModels.Load(ctx.Model); ok && metaModel.QueueRouter != nil {
				// nolint: errcheck
				metaModel.QueueRouter.Route(nil, metaModel.Pods.Array())
			}
		}
	}

	if metaPod.CanLogPodTrace() {
		klog.V(5).InfoS("pod stats updated.", "pod", metaPod.Name, "running requests", requests, "pending load", utilization)
	}
}

func (c *Store) writeRequestTraceToStorage(roundT int64) {
	// Save and reset trace context, atomicity is guaranteed.
	var requestTrace *utils.SyncMap[string, *RequestTrace]
	numTraces := atomic.LoadInt32(&c.numRequestsTraces)
	requestTrace, c.requestTrace = c.requestTrace, &utils.SyncMap[string, *RequestTrace]{}
	numResetTo := int32(0)
	// TODO: Adding a unit test here.
	for !atomic.CompareAndSwapInt32(&c.numRequestsTraces, numTraces, numResetTo) {
		// If new traces added to reset map, assert updatedNumTraces >= numTraces regardless duplication.
		updatedNumTraces := atomic.LoadInt32(&c.numRequestsTraces)
		numTraces, numResetTo = updatedNumTraces, updatedNumTraces-numTraces
	}

	requestTrace.Range(func(modelName string, trace *RequestTrace) bool {
		requestTrace.Store(modelName, nil) // Simply assign nil instead of delete

		trace.Lock()
		pending := int32(0)
		if meta, loaded := c.metaModels.Load(modelName); loaded {
			pending = atomic.LoadInt32(&meta.pendingRequests)
		}
		traceMap := trace.ToMapLocked(pending)
		trace.RecycleLocked()
		trace.Unlock()

		value, err := json.Marshal(traceMap)
		if err != nil {
			klog.ErrorS(err, "error to marshall request trace for redis set")
			return true
		}

		key := fmt.Sprintf("aibrix:%v_request_trace_%v", modelName, roundT)
		if _, err = c.redisClient.Set(context.Background(), key, value, expireWriteRequestTraceIntervalInMins*time.Minute).Result(); err != nil {
			klog.Error(err)
		}
		return true
	})

	klog.V(5).Infof("writeRequestTraceWithKey: %v", roundT)
}

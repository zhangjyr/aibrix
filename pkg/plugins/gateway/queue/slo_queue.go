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

package queue

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

const (
	monogenousGPURouting  bool = false
	initialTotalSubQueues int  = 8  // Expect no more than 8 subqueues
	initialSubQueueSize   int  = 64 // Support maximum 128 pending request per sub-queue within one expansion.
)

type candidateRouterRequest struct {
	*types.RoutingContext
	SubKey     string
	ProfileKey string
	Rank       float64
}

type SLOQueue struct {
	types.Router
	cache cache.Cache

	modelName string
	subs      utils.SyncMap[string, types.RouterQueue[*types.RoutingContext]]
	// features  utils.SyncMap[string, types.RequestFeatures]
	subpool sync.Pool

	dequeueCandidates   []*candidateRouterRequest
	lastCandidateSubKey string
}

func NewSLOQueue(base types.Router, modelName string) (router *SLOQueue, err error) {
	// Dedup deployments
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}

	router = &SLOQueue{
		Router:    base,
		cache:     c,
		modelName: modelName,
	}
	router.subpool.New = func() any { return NewSimpleQueue[*types.RoutingContext](initialSubQueueSize) }
	router.expandDequeueCandidatesLocked(initialTotalSubQueues)
	return router, nil
}

func (q *SLOQueue) Enqueue(ctx *types.RoutingContext, currentTime time.Time) error {
	// Set output predictor first
	if predictor, err := q.cache.GetOutputPredictor(ctx.Model); err != nil {
		return err
	} else {
		ctx.SetOutputPreditor(predictor)
	}

	newQueue := q.subpool.Get().(types.RouterQueue[*types.RoutingContext])
	features, err := ctx.Features()
	if err != nil {
		return err
	}

	key := q.featuresKey(features)
	sub, loaded := q.subs.LoadOrStore(key, newQueue)
	if loaded {
		q.subpool.Put(newQueue)
	}
	// else {
	// 	q.features.Store(key, features)
	// }
	// nolint: errcheck

	sub.Enqueue(ctx, currentTime)
	q.debugSub(fmt.Sprintf("%s request enqueued", ctx.Model))
	return nil
}

func (q *SLOQueue) Peek(currentTime time.Time, pods types.PodList) (*types.RoutingContext, error) {
	// Most implementation goes here.
	var err error

	deployments := pods.Indexes()
	deploymentProfiles := make([]*cache.ModelGPUProfile, len(deployments))
	availableProfiles := 0
	for i, deploymentName := range deployments {
		deploymentProfiles[i], err = q.cache.GetModelProfileByDeploymentName(deploymentName, q.modelName)
		if err != nil {
			klog.Warning(err)
			// Note that deployments[deploymentName] is set and will not try again.
			// We simply ignore this deployment.
			continue
		}
		availableProfiles++
	}
	if availableProfiles == 0 {
		klog.Warningf("no available slo profiles for model %s, fall back to simple queue", q.modelName)
	}
	// Clear error
	err = nil

	// Refill candidates
	q.dequeueCandidates = q.dequeueCandidates[:0]
	q.debugSub(fmt.Sprintf("peeking %s requests", q.modelName))
	// Define fallback handler to handle cases like:
	// 1. No available profiles.
	// 2. Profile does not provide SLO info.
	// Fallback handler emulate a FIFO queue by comparing arrival time
	fallbackHandler := func(key string, subReq *types.RoutingContext) bool {
		if len(q.dequeueCandidates) == 0 {
			q.validateDequeueCandidatesLocked(1)
			q.dequeueCandidates = q.dequeueCandidates[:1]
		} else if q.dequeueCandidates[0].RoutingContext.RequestTime.Before(subReq.RequestTime) {
			// Skip this subqueue
			return true
		}
		// Update ealiest candidate
		q.dequeueCandidates[0].RoutingContext = subReq
		q.dequeueCandidates[0].SubKey = key
		q.dequeueCandidates[0].ProfileKey = ""
		return true
	}
	q.subs.Range(func(key string, sub types.RouterQueue[*types.RoutingContext]) bool {
		if sub.Len() > 0 {
			r, _ := sub.Peek(currentTime, pods)
			// Keep fallback decision in case anything wrong.
			fbRet := fallbackHandler(key, r)
			// Fallback case 1: No available profiles.
			if availableProfiles == 0 {
				klog.Warningf("SLOQueue found no profile available for request %s of model %s, fallback to FIFO queue", r.RequestID, r.Model)
				return fbRet
			}
			// Reuse a dequeue candidate
			idx := len(q.dequeueCandidates)
			q.validateDequeueCandidatesLocked(idx + 1)
			q.dequeueCandidates = q.dequeueCandidates[:idx+1]
			// Reset values
			q.dequeueCandidates[idx].RoutingContext = r
			q.dequeueCandidates[idx].SubKey = key
			q.dequeueCandidates[idx].ProfileKey = ""
			// Use a relaxer SLO
			for i, profile := range deploymentProfiles {
				// Skip deployments with no profile
				if profile == nil {
					continue
				}
				rank, rankErr := q.rank(currentTime, r, profile)
				// Fallback case 2: Profile does not provide SLO info.
				if rankErr != nil {
					err = rankErr
					klog.Warningf("SLOQueue failed to get SLO info for request %s with profile %s: %v, fallback to FIFO queue", r.RequestID, profile.Deployment, rankErr)
					return fbRet
				}
				// if q.queueOverallSLO {
				// 	rank = q.queueRank(currentTime, &q.Subs[i], j)
				// }
				if len(q.dequeueCandidates[idx].ProfileKey) == 0 || q.higherRank(rank, q.dequeueCandidates[idx].Rank) < 0 {
					q.dequeueCandidates[idx].ProfileKey = deployments[i]
					q.dequeueCandidates[idx].Rank = rank
				}
			}
		}
		return true
	})
	if err != nil {
		// Apply fallback decision by just keep the first one.
		q.dequeueCandidates = q.dequeueCandidates[:1]
	}

	if len(q.dequeueCandidates) == 0 {
		return nil, types.ErrQueueEmpty
	} else if len(q.dequeueCandidates) == 1 {
		// Only candidate
		q.lastCandidateSubKey = q.dequeueCandidates[0].SubKey
		return q.dequeueCandidates[0].RoutingContext, nil
	}

	// Sort by rank
	sort.Slice(q.dequeueCandidates, func(i, j int) bool {
		// Keep original order for no slo violation
		if q.dequeueCandidates[i].Rank < 0 && q.dequeueCandidates[j].Rank < 0 {
			return q.dequeueCandidates[i].RoutingContext.RequestTime.Before(q.dequeueCandidates[j].RoutingContext.RequestTime)
		} else {
			return q.higherRank(q.dequeueCandidates[i].Rank, q.dequeueCandidates[j].Rank) > 0
		}
	})

	// Start from ealiest
	for _, candidate := range q.dequeueCandidates {
		if monogenousGPURouting {
			//nolint:errcheck
			q.Router.Route(candidate.RoutingContext, &utils.PodArray{Pods: pods.ListByIndex(candidate.ProfileKey)})
		} else {
			//nolint:errcheck
			q.Router.Route(candidate.RoutingContext, pods)
		}
		if candidate.RoutingContext.HasRouted() {
			q.lastCandidateSubKey = candidate.SubKey
			return candidate.RoutingContext, nil
		}
	}

	return nil, nil
}

func (q *SLOQueue) Dequeue(ts time.Time) (*types.RoutingContext, error) {
	if len(q.lastCandidateSubKey) == 0 {
		return nil, fmt.Errorf("call SLOQueue.Peek first")
	}
	sub, _ := q.subs.Load(q.lastCandidateSubKey)
	q.lastCandidateSubKey = ""
	defer q.debugSub(fmt.Sprintf("%s request dequeued from sub %s", q.modelName, q.lastCandidateSubKey))
	return sub.Dequeue(ts)
}

func (q *SLOQueue) Len() (total int) {
	q.subs.Range(func(_ string, sub types.RouterQueue[*types.RoutingContext]) bool {
		total += sub.Len()
		return true
	})
	return
}

func (q *SLOQueue) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	// Ctx is not routed if no profiles is found during Peek.
	if !ctx.HasRouted() {
		return q.Router.Route(ctx, pods)
	}
	return ctx.TargetAddress(), nil
}

func (q *SLOQueue) validateDequeueCandidatesLocked(size int) {
	if size <= cap(q.dequeueCandidates) {
		return
	}

	q.expandDequeueCandidatesLocked(0)
}

func (q *SLOQueue) expandDequeueCandidatesLocked(limit int) {
	if limit == 0 {
		limit = 2 * cap(q.dequeueCandidates)
	}
	if limit < initialTotalSubQueues {
		limit = initialTotalSubQueues
	}
	candidates := make([]*candidateRouterRequest, limit)
	var dequeueCandidates []*candidateRouterRequest
	if cap(q.dequeueCandidates) > 0 {
		dequeueCandidates = q.dequeueCandidates[:cap(q.dequeueCandidates)]
		copy(candidates, dequeueCandidates)
	}
	for i := len(dequeueCandidates); i < len(candidates); i++ {
		candidates[i] = &candidateRouterRequest{}
	}
	q.dequeueCandidates = candidates[:len(q.dequeueCandidates)]
}

func (q *SLOQueue) featuresKey(features types.RequestFeatures) string {
	for i := range features {
		features[i] = math.Round(math.Log2(features[i]))
	}
	return fmt.Sprint(features)
}

func (q *SLOQueue) rank(currentTime time.Time, req *types.RoutingContext, profile *cache.ModelGPUProfile) (rank float64, err error) {
	features, err := req.Features() // Since req are in the queue, Features() must be called before without an error.
	if err != nil {
		return 0.0, err
	}
	signature, err := profile.GetSignature(features...)
	if err != nil {
		return 0.0, err
	}
	if profile.SLOs.TTFT > 0.0 {
		rank, err = q.rankTTFT(currentTime, req, profile, signature)
		if err != nil {
			return 0.0, err
		}
	} else {
		rank, err = q.rankE2E(currentTime, req, profile, signature)
		if err != nil {
			return 0.0, err
		}
	}
	return rank, nil
}

func (q *SLOQueue) rankE2E(currentTime time.Time, req *types.RoutingContext, profile *cache.ModelGPUProfile, signature []int) (float64, error) {
	target := profile.SLOs.E2E // TODO: We should make SLO.E2E always available
	expected, err := profile.LatencySeconds(signature...)
	if err != nil {
		return 0.0, err
	}
	return float64(currentTime.Sub(req.RequestTime))/float64(time.Second) + expected - target, nil
}

func (q *SLOQueue) rankTTFT(currentTime time.Time, req *types.RoutingContext, profile *cache.ModelGPUProfile, signature []int) (float64, error) {
	target := profile.SLOs.TTFT
	expected, err := profile.TTFTSeconds(signature...)
	if err != nil {
		return 0.0, err
	}
	return float64(currentTime.Sub(req.RequestTime))/float64(time.Second) + expected - target, nil
}

// func (q *SLOQueue) queueRank(currentTime float64, sub *SimpleQueue, profileIdx int) float64 {
// 	firstRouterRequest := sub.Queue[0]
// 	waitTime := currentTime - firstRouterRequest.ArrivalTime
// 	serviceTime := firstRouterRequest.ServiceTime(profileIdx)
// 	if q.useProfileServiceTime {
// 		serviceTime = fixed(q.providers[firstRouterRequest.ProducerID].Profiles[profileIdx].Mu) // Simulate that accurate service time is unknown.
// 	}
// 	queueServiceTime := float64(len(sub.Queue)) / q.providers[firstRouterRequest.ProducerID].Profiles[profileIdx].ULambda
// 	return waitTime + math.Max(serviceTime, queueServiceTime) - q.providers[firstRouterRequest.ProducerID].SLO
// }

func (q *SLOQueue) higherRank(rank1 float64, rank2 float64) float64 {
	return rank1 - rank2
}

func (q *SLOQueue) debugSub(msg string) {
	if !klog.V(4).Enabled() {
		return
	}

	var logMsg strings.Builder
	logMsg.WriteString(msg)
	logMsg.WriteRune(',')
	logMsg.WriteString("SLOQueue subs stats:")
	q.subs.Range(func(key string, sub types.RouterQueue[*types.RoutingContext]) bool {
		logMsg.WriteString(key)
		logMsg.WriteRune('(')
		logMsg.WriteString(strconv.Itoa(sub.Len()))
		logMsg.WriteRune(')')
		return true
	})
	klog.V(4).Infof(logMsg.String())
}

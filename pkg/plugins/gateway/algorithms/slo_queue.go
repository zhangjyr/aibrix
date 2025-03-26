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
	"math"
	"sort"
	"sync"
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

const (
	monogenousGPURouting bool = false
)

type candidateRouterRequest struct {
	*types.RoutingContext
	SubKey     string
	ProfileKey string
	Rank       float64
}

type SLOQueue struct {
	types.Router

	modelName string
	subs      utils.SyncMap[string, *types.SimpleQueue[*types.RoutingContext]]
	features  utils.SyncMap[string, types.RequestFeatures]
	subpool   sync.Pool

	dequeueCandidates []*candidateRouterRequest
	lastSubKey        string
	lastPodCandidate  string
}

func NewSLOQueue(base types.Router, modelName string) (router *SLOQueue) {
	router = &SLOQueue{
		Router:    base,
		modelName: modelName,
	}
	router.subpool.New = func() any { return &types.SimpleQueue[*types.RoutingContext]{} }
	router.expandDequeueCandidatesLocked(10) // Arbitarily reserve 10 slots
	return router
}

func (q *SLOQueue) Enqueue(c *types.RoutingContext, currentTime time.Time) error {
	newQueue := q.subpool.Get().(*types.SimpleQueue[*types.RoutingContext])
	features, err := c.Features()
	if err != nil {
		return err
	}

	key := q.featuresKey(features)
	sub, loaded := q.subs.LoadOrStore(key, newQueue)
	if loaded {
		q.subpool.Put(newQueue)
	} else {
		q.features.Store(key, features)
	}
	sub.Enqueue(c, currentTime)
	return nil
}

func (q *SLOQueue) Peek(currentTime time.Time, pods types.PodList) (req *types.RoutingContext, err error) {
	// Most implementation goes here.

	// Dedup deployments
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}
	deployments := pods.Indexes()
	deploymentProfiles := make([]*cache.ModelGPUProfile, len(deployments))
	for i, deploymentName := range deployments {
		deploymentProfiles[i], err = c.GetModelProfileByDeploymentName(deploymentName, q.modelName)
		if err != nil {
			klog.Error(err)
			// Note that deployments[deploymentName] is set and will not try again.
			// We simply ignore this deployment.
		}
	}

	// Refill candidates
	q.dequeueCandidates = q.dequeueCandidates[:0]
	q.subs.Range(func(key string, sub *types.SimpleQueue[*types.RoutingContext]) bool {
		if sub.Len() > 0 {
			r, _ := sub.Peek(currentTime, pods)
			// Reuse a dequeue candidate
			idx := len(q.dequeueCandidates)
			q.validateDequeueCandidatesLocked(idx + 1)
			q.dequeueCandidates = q.dequeueCandidates[:idx+1]
			// Reset values
			q.dequeueCandidates[idx].RoutingContext = req
			q.dequeueCandidates[idx].SubKey = key
			q.dequeueCandidates[idx].ProfileKey = ""
			// Use a relaxer SLO
			for i, profile := range deploymentProfiles {
				// Skip deployments with no profile
				if profile == nil {
					continue
				}
				rank, rankErr := q.rank(currentTime, r, profile)
				if rankErr != nil {
					err = rankErr
					return false
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
		return nil, err
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
	q.lastPodCandidate = ""
	for _, candidate := range q.dequeueCandidates {
		q.lastSubKey = candidate.SubKey
		if monogenousGPURouting {
			q.lastPodCandidate, _ = q.Router.Route(candidate.RoutingContext, &utils.PodArray{Pods: pods.ListByIndex(candidate.ProfileKey)})
		} else {
			q.lastPodCandidate, _ = q.Router.Route(candidate.RoutingContext, pods)
		}
		if len(q.lastPodCandidate) != 0 {
			return candidate.RoutingContext, nil
		}
	}

	return nil, nil
}

func (q *SLOQueue) Dequeue() (*types.RoutingContext, error) {
	if len(q.lastPodCandidate) == 0 {
		return nil, fmt.Errorf("call SLOQueue.Peek first")
	}
	sub, _ := q.subs.Load(q.lastSubKey)
	q.lastSubKey = ""
	q.lastPodCandidate = ""
	return sub.Dequeue()
}

func (q *SLOQueue) Len() (total int) {
	q.subs.Range(func(_ string, sub *types.SimpleQueue[*types.RoutingContext]) bool {
		total += sub.Len()
		return true
	})
	return
}

func (q *SLOQueue) Route(ctx *types.RoutingContext, pods types.PodList) (podIP string, err error) {
	if len(q.lastPodCandidate) == 0 {
		return "", fmt.Errorf("call SLOQueue.Peek first")
	}
	return q.lastPodCandidate, nil
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
	candidates := make([]*candidateRouterRequest, limit)
	var dequeueCandidates []*candidateRouterRequest
	if cap(q.dequeueCandidates) > 0 {
		dequeueCandidates = q.dequeueCandidates[:cap(q.dequeueCandidates)]
		copy(candidates, dequeueCandidates)
	}
	for i := len(dequeueCandidates); i < len(candidates); i++ {
		dequeueCandidates[i] = &candidateRouterRequest{}
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
	features, _ := req.Features() // Since req are in the queue, Features() must be called before without an error.
	signature, _ := profile.GetSignature(features...)
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
	target := profile.SLOs.E2E // TODO: We should make  SLO.E2E always available
	expected, err := profile.LatencySeconds(signature...)
	if err != nil {
		return 0.0, err
	}
	return float64(currentTime.Sub(req.RequestTime))/float64(time.Second) + expected - target, nil
}

func (q *SLOQueue) rankTTFT(currentTime time.Time, req *types.RoutingContext, profile *cache.ModelGPUProfile, signature []int) (float64, error) {
	target := profile.SLOs.TTFT // TODO: We should make  SLO.E2E always available
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

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
	"math"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const MovingInterval = 10 * time.Second

type SimmpleOutputPredictor struct {
	history       rotatingHistory
	inputs        outputDistribution
	inputsSums    []int32
	inputBuckets  int
	outputBuckets int

	mu sync.RWMutex
}

type outputDistribution []int32 // Inputs:Output distribution

func (hist outputDistribution) reset(distributions outputDistribution, sums []int32, outputBuckets int) {
	inputBucket := 0
	leftOutputBucket := outputBuckets
	for i := range hist {
		atomic.AddInt32(&sums[inputBucket], -hist[i])
		atomic.AddInt32(&distributions[i], -hist[i])
		hist[i] = 0
		leftOutputBucket--
		if leftOutputBucket == 0 {
			inputBucket++
			leftOutputBucket = outputBuckets
		}
	}
}

type rotatingHistory struct {
	window        []outputDistribution
	head          int
	tail          int
	headTimestamp time.Time
}

func (hist *rotatingHistory) forwardLocked(ts time.Time) int {
	if ts.Sub(hist.headTimestamp) < MovingInterval {
		return 0
	}

	forwarded := 0
	newHeadTimestamp := hist.headTimestamp
	for ts.Sub(newHeadTimestamp) >= MovingInterval {
		forwarded++
		newHeadTimestamp = newHeadTimestamp.Add(MovingInterval)
	}
	// Assert: new head is reset.
	hist.head = (hist.head + 1) % len(hist.window)
	hist.headTimestamp = newHeadTimestamp

	return forwarded
}

func NewSimmpleOutputPredictor(maxInputTokens, maxOutputTokens int, window time.Duration) *SimmpleOutputPredictor {
	// We allocate 1 more history slot to make summary update on rotating lock free
	extraSlot := 1
	if window%MovingInterval > 0 {
		extraSlot++
	}
	inputBuckets := int(math.Ceil(math.Log2(float64(maxInputTokens))))
	outputBuckets := int(math.Ceil(math.Log2(float64(maxOutputTokens))))
	predictor := &SimmpleOutputPredictor{
		history: rotatingHistory{
			window:        make([]outputDistribution, int(window/MovingInterval)+extraSlot),
			headTimestamp: time.Now(),
		},
		inputs:        make(outputDistribution, inputBuckets*outputBuckets),
		inputsSums:    make([]int32, inputBuckets),
		inputBuckets:  inputBuckets,
		outputBuckets: outputBuckets,
	}
	for i := 0; i < len(predictor.history.window); i++ {
		predictor.history.window[i] = make(outputDistribution, inputBuckets*outputBuckets)
	}
	return predictor
}

func (p *SimmpleOutputPredictor) AddTrace(inputTokens, outputTokens int, cnt int32) {
	p.tryRotate()

	idx := p.bucket2idx(p.token2bucket(inputTokens, p.inputBuckets), p.token2bucket(outputTokens, p.outputBuckets))

	// Avoid operations during rotating
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Add summary first and history next to avoid possible negative summary on rotating.
	atomic.AddInt32(&p.inputs[idx], cnt)
	atomic.AddInt32(&p.history.window[p.history.head][idx], cnt)
}

func (p *SimmpleOutputPredictor) Predict(inputTokens int) int {
	inputBucket := p.token2bucket(inputTokens, p.inputBuckets)
	// Do weighted random
	cursor := rand.Int31n(atomic.LoadInt32(&p.inputsSums[inputBucket]))
	accumulation := int32(0)
	scanRange := (inputBucket + 1) * p.outputBuckets
	for i := scanRange - p.outputBuckets; i < scanRange; i++ {
		accumulation += atomic.LoadInt32(&p.inputs[i])
		if cursor < accumulation {
			return int(math.Pow(2, float64(i-scanRange+p.outputBuckets)))
		}
	}
	return int(math.Pow(2, float64(p.outputBuckets-1)))
}

func (p *SimmpleOutputPredictor) bucket2idx(inputBucket, outputBucket int) int {
	return inputBucket*p.inputBuckets + outputBucket
}

func (p *SimmpleOutputPredictor) token2bucket(tokens int, limit int) int {
	bucket := 0
	if tokens > 0 {
		bucket = int(math.Round(math.Log2(float64(tokens))))
	}
	if bucket >= limit {
		bucket = limit - 1
	}
	return bucket
}

func (p *SimmpleOutputPredictor) tryRotate() {
	ts := time.Now()

	if ts.Sub(p.history.headTimestamp) < MovingInterval {
		return
	}
	go p.rotate(ts)
	runtime.Gosched() // allow rotate first.
}

func (p *SimmpleOutputPredictor) rotate(ts time.Time) {
	p.mu.Lock()
	tail := p.history.tail // Keep a copy to make multiple reference consistent
	spares := tail - p.history.head
	if spares < 0 {
		spares = tail + len(p.history.window) - p.history.head
	}
	if spares < 1 {
		// No spare history slot, abandon the current attempt.
		p.mu.Unlock()
		return
	}
	// Calculate how many intervals we need to forward.
	// This is usually 1, for sparse workloads, this can be > 1.
	forwarded := p.history.forwardLocked(ts)
	p.mu.Unlock()

	// We keep at least 1 spare history (so summary can be updated concurrently with new data)
	if spares > forwarded {
		return
	}

	// Remove olded data from summary and reset history of number min(forwarded, len(p.history.window) - 1)
	// Noted the read window size is len(p.history.window) - 1, and extra one is for new data only.
	for i := 0; i < forwarded && i < len(p.history.window)-1; i++ {
		p.history.window[p.history.tail].reset(p.inputs, p.inputsSums, p.outputBuckets)
		p.history.tail = (p.history.tail + 1) % len(p.history.window)
	}
}

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
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vllm-project/aibrix/pkg/types"
)

var (
	ErrZeroValueNotSupported = errors.New("zero value not supported")
)

type SimpleQueue[V comparable] struct {
	mu            sync.RWMutex
	queue         []V
	enqueueCursor int32 // Atomic
	dequeueCursor int32 // Atomic

	// expansion footprints
	expansionOffset int32
}

func NewSimpleQueue[V comparable](initialCapacity int) *SimpleQueue[V] {
	if initialCapacity < 1 {
		initialCapacity = types.DefaultQueueCapacity
	}
	return &SimpleQueue[V]{
		queue: make([]V, initialCapacity),
	}
}

func (q *SimpleQueue[V]) Enqueue(value V, _ time.Time) error {
	var zero V
	if value == zero {
		return ErrZeroValueNotSupported
	}

	q.mu.RLock()
	defer q.mu.RUnlock()

	pos := atomic.AddInt32(&q.enqueueCursor, 1)
	capacity := int32(len(q.queue))

	fixedPos := pos
	if pos > capacity {
		q.mu.RUnlock()
		fixedPos = q.expand(pos)
		q.mu.RLock()
	}
	q.queue[fixedPos-1] = value
	return nil
}

func (q *SimpleQueue[V]) Peek(_ time.Time, _ types.PodList) (c V, err error) {
	q.mu.RLock()
	defer q.mu.RUnlock()

	dequeuePos := atomic.LoadInt32(&q.dequeueCursor)
	enqueuePos := atomic.LoadInt32(&q.enqueueCursor)

	if dequeuePos >= enqueuePos {
		return c, types.ErrQueueEmpty
	}

	return q.queue[dequeuePos], nil
}

func (q *SimpleQueue[V]) Dequeue(_ time.Time) (c V, err error) {
	q.mu.RLock()
	defer q.mu.RUnlock()

	nilVal := c
	for {
		dequeuePos := atomic.LoadInt32(&q.dequeueCursor)
		enqueuePos := atomic.LoadInt32(&q.enqueueCursor)

		if dequeuePos >= enqueuePos {
			return c, types.ErrQueueEmpty
		}

		// Like enqueue, dequeuePos can out of bound along with enqueuePos
		if dequeuePos >= int32(len(q.queue)) {
			q.mu.RUnlock()
			dequeuePos = q.expand(dequeuePos)
			q.mu.RLock()
		}

		// Make sure value was completely enqueued or return err.
		// This is rare. Instead of wait for evaluation, we treat it as queue empty.
		c = q.queue[dequeuePos]
		if c == nilVal {
			return c, types.ErrQueueEmpty
		}
		if atomic.CompareAndSwapInt32(&q.dequeueCursor, dequeuePos, dequeuePos+1) {
			// Release reference
			q.queue[dequeuePos] = nilVal
			return c, nil
		} else {
			// reset
			c = nilVal
		}
	}
}

func (q *SimpleQueue[V]) Len() int {
	q.mu.RLock()
	defer q.mu.RUnlock()

	return int(atomic.LoadInt32(&q.enqueueCursor) - atomic.LoadInt32(&q.dequeueCursor))
}

func (q *SimpleQueue[V]) Cap() int {
	q.mu.RLock()
	defer q.mu.RUnlock()

	return cap(q.queue)
}

func (q *SimpleQueue[V]) expand(triggerCursor int32) int32 {
	q.mu.Lock()
	defer q.mu.Unlock()

	if triggerCursor < int32(len(q.queue)) {
		return triggerCursor - q.expansionOffset
	}

	q.expansionOffset = 0 // Reset offset
	oldCapacity := int32(cap(q.queue))
	used := triggerCursor - q.dequeueCursor - 1

	// Determine new capacity
	newQueue := q.queue
	if used > oldCapacity/2 {
		// Expand capacity
		newQueue = make([]V, oldCapacity*2)
	}
	// Pack existing elements
	copy(newQueue, q.queue[q.dequeueCursor:])
	q.expansionOffset = q.dequeueCursor
	q.queue, q.enqueueCursor, q.dequeueCursor = newQueue, q.enqueueCursor-q.expansionOffset, 0
	return triggerCursor - q.expansionOffset
}

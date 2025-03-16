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

package utils

import (
	"time"
)

// GlobalQueue defined the global queue for the gateway.
// Enqueue is thread-safe, while peek and dequeue is not.
type GlobalQueue[V comparable] interface {
	Enqueue(V, time.Time) error
	Peek(time.Time, *PodArray) (V, error)
	Dequeue() (V, error)
	Len() int
}

type SimpleQueue[V comparable] struct {
	buffer chan V
	head   V
}

func NewSimpleQueue[V comparable](cap int) *SimpleQueue[V] {
	return &SimpleQueue[V]{
		buffer: make(chan V, cap),
	}
}

func (q *SimpleQueue[V]) Enqueue(c V, _ time.Time) error {
	// c.LastEnqueueTime = currentTime
	q.buffer <- c
	return nil
}

func (q *SimpleQueue[V]) Peek(_ time.Time, _ *PodArray) (c V, err error) {
	if q.head == c && len(q.buffer) == 0 {
		return c, nil
	} else if q.head == c {
		q.head = <-q.buffer
	}
	return q.head, nil
}

func (q *SimpleQueue[V]) Dequeue() (c V, err error) {
	empty := c
	c, err = q.Peek(time.Time{}, nil)
	q.head = empty
	return
}

func (q *SimpleQueue[V]) Len() int {
	var empty V
	if q.head == empty {
		return len(q.buffer)
	} else {
		return len(q.buffer) + 1
	}
}

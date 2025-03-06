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

type GlobalQueue[V any] interface {
	Enqueue(V, time.Time) error
	Peek(time.Time, *PodArray) (V, error)
	Dequeue() (V, error)
	Len() int
}

type SimpleQueue[V any] struct {
	queue []V
}

func (q *SimpleQueue[V]) Enqueue(c V, _ time.Time) error {
	// c.LastEnqueueTime = currentTime
	q.queue = append(q.queue, c)
	return nil
}

func (q *SimpleQueue[V]) Peek(_ time.Time, _ *PodArray) (c V, err error) {
	if len(q.queue) == 0 {
		return
	}
	return q.queue[0], nil
}

func (q *SimpleQueue[V]) Dequeue() (c V, err error) {
	if len(q.queue) == 0 {
		return
	}
	c = q.queue[0]
	q.queue = q.queue[1:]
	return
}

func (q *SimpleQueue[V]) Len() int {
	return len(q.queue)
}

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
	"sync"
)

type Registry[V any] struct {
	registry      map[string]V
	values        []V // Pods cache for quick iteration
	cacheArr      any
	cacheProvider func([]V) any
	mu            sync.RWMutex
}

func NewRegistry[V any]() *Registry[V] {
	return &Registry[V]{}
}

func NewRegistryWithArrayProvider[V any](provider func([]V) any) *Registry[V] {
	return &Registry[V]{
		cacheProvider: provider,
	}
}

func (reg *Registry[V]) Delete(key string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	if reg.registry == nil {
		return
	}

	delete(reg.registry, key)
	reg.values = nil
	reg.cacheArr = nil
}

func (reg *Registry[V]) Load(key string) (value V, ok bool) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()

	value, ok = reg.registry[key]
	return
}

func (reg *Registry[V]) Store(key string, value V) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	if reg.registry == nil {
		reg.registry = make(map[string]V, 1)
	}

	reg.registry[key] = value
	reg.values = append(reg.values, value)
	reg.cacheArr = nil
}

func (reg *Registry[V]) Array() (arr []V) {
	arr = reg.values
	if arr != nil || reg.registry == nil {
		return arr
	}

	// Reconstruct array
	arr, _ = reg.updateArrayLocked()
	return
}

func (reg *Registry[V]) CustomizedArray() (arr any) {
	arr = reg.cacheArr
	if arr != nil {
		return arr
	}

	// Reconstruct array
	_, arr = reg.updateArrayLocked()
	return
}

func (reg *Registry[V]) Len() int {
	pods := reg.values
	if pods != nil {
		return len(pods)
	}

	reg.mu.RLock()
	defer reg.mu.RUnlock()

	return len(reg.registry)
}

func (reg *Registry[V]) updateArrayLocked() (arr []V, cached any) {
	reconstructed := false
	if reg.values == nil && reg.registry != nil {
		reg.values = make([]V, 0, len(reg.registry))
		for _, pod := range reg.registry {
			reg.values = append(reg.values, pod)
		}
		reconstructed = true
	}

	if reg.cacheProvider != nil && (reconstructed || reg.cacheArr == nil) {
		reg.cacheArr = reg.cacheProvider(reg.values)
	}

	return reg.values, reg.cacheArr
}

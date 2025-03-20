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
	registry map[string]V
	values   []V  // Pods cache for quick iteration
	valid    bool // If value valid?
	mu       sync.RWMutex
}

type CustomizedRegistry[V any, A comparable] struct {
	Registry[V]
	values         A // Pods cache for quick iteration
	valuesProvider func([]V) A
}

func NewRegistry[V any]() *Registry[V] {
	return &Registry[V]{
		valid: true,
	}
}

func NewRegistryWithArrayProvider[V any, A comparable](provider func([]V) A) *CustomizedRegistry[V, A] {
	return &CustomizedRegistry[V, A]{
		Registry:       Registry[V]{},
		valuesProvider: provider,
	}
}

func (reg *Registry[V]) Delete(key string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	if reg.registry == nil {
		return
	}

	delete(reg.registry, key)
	// Check stale
	if len(reg.values) != len(reg.registry) {
		reg.values, reg.valid = reg.values[:0], false // atomic set, Reuse base array
	}
}

func (reg *CustomizedRegistry[V, A]) Delete(key string) {
	var nilVal A
	reg.values = nilVal
	reg.Registry.Delete(key)
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

	_, exist := reg.registry[key]
	stale := len(reg.values) != len(reg.registry)
	reg.registry[key] = value
	if !exist && !stale {
		reg.values, reg.valid = append(reg.values, value), true // atomic set
	} else {
		// clear and wait regenerate
		reg.values, reg.valid = reg.values[:0], false
	}
}

func (reg *CustomizedRegistry[V, A]) Store(key string, value V) {
	var nilVal A
	reg.values = nilVal
	reg.Registry.Store(key, value)
}

func (reg *Registry[V]) Array() (arr []V) {
	arr = reg.values
	if arr != nil || reg.registry == nil {
		return arr
	}

	reg.mu.Lock()
	defer reg.mu.Unlock()

	// Reconstruct array
	arr, _ = reg.updateArrayLocked()
	return arr
}

func (reg *CustomizedRegistry[V, A]) Array() (arr A) {
	ret := reg.values
	if ret != arr { // ret != nil value
		return ret
	}

	reg.mu.Lock()
	defer reg.mu.Unlock()

	// Reconstruct array
	return reg.updateArrayLocked()
}

func (reg *Registry[V]) Len() int {
	pods, valid := reg.values, reg.valid // atomic
	if valid {
		return len(pods)
	}

	reg.mu.RLock()
	defer reg.mu.RUnlock()

	return len(reg.registry)
}

func (reg *Registry[V]) updateArrayLocked() ([]V, bool) {
	reconstructed := false
	if !reg.valid && reg.registry != nil {
		if cap(reg.values) < len(reg.registry) {
			reg.values = make([]V, 0, len(reg.registry))
		}
		for _, pod := range reg.registry {
			reg.values = append(reg.values, pod)
		}
		reconstructed = true
	}

	return reg.values, reconstructed
}

func (reg *CustomizedRegistry[V, A]) updateArrayLocked() (val A) {
	values, reconstructed := reg.Registry.updateArrayLocked()
	// Unlike slice: nil can be treated as empty []V, we always create empty A even values is nil
	if reconstructed || reg.values == val { // reg.values == nil val
		reg.values = reg.valuesProvider(values)
	}
	return reg.values
}

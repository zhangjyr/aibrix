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
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
	"go.uber.org/atomic"
	v1 "k8s.io/api/core/v1"
)

type Pod struct {
	*v1.Pod
	Models       *utils.Registry[string]                    // Model/adapter names that the pod is running
	Metrics      utils.SyncMap[string, metrics.MetricValue] // Pod metrics (metric_name -> value)
	ModelMetrics utils.SyncMap[string, metrics.MetricValue] // Pod-model metrics (model_name/metric_name -> value)

	runningRequests        int32 // Realtime running requests counter.
	pendingLoadUtilization atomic.Float64
}

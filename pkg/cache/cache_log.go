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
	"strings"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"k8s.io/klog/v2"
)

func (c *Store) debugInfo() {
	c.metaPods.Range(func(podName string, pod *Pod) bool {
		klog.V(4).Infof("pod: %s, podIP: %v, models: %s", podName, pod.Status.PodIP, strings.Join(pod.Models.Array(), " "))
		pod.Metrics.Range(func(metricName string, metricVal metrics.MetricValue) bool {
			klog.V(5).Infof("%v_%v_%v", podName, metricName, metricVal)
			return true
		})
		pod.ModelMetrics.Range(func(metricName string, metricVal metrics.MetricValue) bool {
			klog.V(5).Infof("%v_%v_%v", podName, metricName, metricVal)
			return true
		})
		return true
	})
	c.metaModels.Range(func(modelName string, meta *Model) bool {
		var podList strings.Builder
		for _, pod := range meta.Pods.Registry.Array() {
			podList.WriteString(pod.Name)
			podList.WriteByte(' ')
		}
		klog.V(4).Infof("model: %s, pods: %v", modelName, podList)
		return true
	})
}

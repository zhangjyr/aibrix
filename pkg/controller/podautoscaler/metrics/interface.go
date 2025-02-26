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

package metrics

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"

	v1 "k8s.io/api/core/v1"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
)

// NamespaceNameMetric contains the namespace, name and the metric name
type NamespaceNameMetric struct {
	types.NamespacedName // target deployment namespace and name
	MetricName           string
	PaNamespace          string
	PaName               string
}

// NewNamespaceNameMetric creates a NamespaceNameMetric based on the PodAutoscaler's metrics source.
// For consistency, it will return the corresponding MetricSource.
// Currently, it supports only a single metric source. In the future, this could be extended to handle multiple metric sources.
func NewNamespaceNameMetric(pa *autoscalingv1alpha1.PodAutoscaler) (NamespaceNameMetric, autoscalingv1alpha1.MetricSource, error) {
	if len(pa.Spec.MetricsSources) != 1 {
		return NamespaceNameMetric{}, autoscalingv1alpha1.MetricSource{}, fmt.Errorf("metrics sources must be 1, but got %d", len(pa.Spec.MetricsSources))
	}
	metricSource := pa.Spec.MetricsSources[0]
	return NamespaceNameMetric{
		NamespacedName: types.NamespacedName{
			Namespace: pa.Namespace,
			Name:      pa.Spec.ScaleTargetRef.Name,
		},
		MetricName:  metricSource.TargetMetric,
		PaNamespace: pa.Namespace,
		PaName:      pa.Name,
	}, metricSource, nil
}

// PodMetric contains pod metric value (the metric values are expected to be the metric as a milli-value)
type PodMetric struct {
	Timestamp time.Time
	// kubernetes metrics return this value.
	Window          time.Duration
	Value           int64
	MetricsName     string
	containerPort   int32
	ScaleObjectName string
}

// PodMetricsInfo contains pod metrics as a map from pod names to PodMetricsInfo
type PodMetricsInfo map[string]PodMetric

// MetricClient knows how to query a remote interface to retrieve container-level
// resource metrics as well as pod-level arbitrary metrics
type MetricClient interface {
	// GetPodContainerMetric gets the given resource metric (and an associated oldest timestamp)
	// for the specified named container in specific pods in the given namespace and when
	// the container is an empty string it returns the sum of all the container metrics.
	// TODO: should we use `metricKey` all the time?
	GetPodContainerMetric(ctx context.Context, pod v1.Pod, source autoscalingv1alpha1.MetricSource) (PodMetricsInfo, time.Time, error)

	GetMetricsFromPods(ctx context.Context, pods []v1.Pod, source autoscalingv1alpha1.MetricSource) ([]float64, error)

	GetMetricFromSource(ctx context.Context, source autoscalingv1alpha1.MetricSource) (float64, error)

	// Obsoleted, please use UpdateMetrics
	UpdatePodListMetric(metricValues []float64, metricKey NamespaceNameMetric, now time.Time) error

	UpdateMetrics(now time.Time, metricKey NamespaceNameMetric, metricValues ...float64) error
}

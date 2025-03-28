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
	"context"
	"fmt"

	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	NAMESPACE = "aibrix-system"
)

// IsPodTerminating check if pod is in terminating status via whether the deletion timestamp is set
func IsPodTerminating(pod *v1.Pod) bool {
	return pod.ObjectMeta.DeletionTimestamp != nil
}

// IsPodReady returns true if a pod is ready; false otherwise.
func IsPodReady(pod *v1.Pod) bool {
	return IsPodReadyConditionTrue(pod.Status)
}

// IsPodReadyConditionTrue returns true if a pod is ready; false otherwise.
func IsPodReadyConditionTrue(status v1.PodStatus) bool {
	condition := GetPodReadyCondition(status)
	return condition != nil && condition.Status == v1.ConditionTrue
}

// GetPodReadyCondition extracts the pod ready condition from the given status and returns that.
// Returns nil if the condition is not present.
func GetPodReadyCondition(status v1.PodStatus) *v1.PodCondition {
	_, condition := GetPodCondition(&status, v1.PodReady)
	return condition
}

// GetPodCondition extracts the provided condition from the given status and returns that.
// Returns nil and -1 if the condition is not present, and the index of the located condition.
func GetPodCondition(status *v1.PodStatus, conditionType v1.PodConditionType) (int, *v1.PodCondition) {
	if status == nil {
		return -1, nil
	}
	return GetPodConditionFromList(status.Conditions, conditionType)
}

// GetPodConditionFromList extracts the provided condition from the given list of condition and
// returns the index of the condition and the condition. Returns -1 and nil if the condition is not present.
func GetPodConditionFromList(conditions []v1.PodCondition, conditionType v1.PodConditionType) (int, *v1.PodCondition) {
	if conditions == nil {
		return -1, nil
	}
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return i, &conditions[i]
		}
	}
	return -1, nil
}

// SetConditionInList sets the specific condition type on the given PodAutoscaler to the specified value with the given
// reason and message.
// The message and args are treated like a format string.
// The condition will be added if it is not present. The new list will be returned.
func SetConditionInList(inputList []metav1.Condition, conditionType string, status metav1.ConditionStatus, reason, message string, args ...interface{}) []metav1.Condition {
	resList := inputList
	var existingCond *metav1.Condition
	for i, condition := range resList {
		if condition.Type == conditionType {
			// can't take a pointer to an iteration variable
			existingCond = &resList[i]
			break
		}
	}

	if existingCond == nil {
		resList = append(resList, metav1.Condition{
			Type: conditionType,
		})
		existingCond = &resList[len(resList)-1]
	}

	if existingCond.Status != status {
		existingCond.LastTransitionTime = metav1.Now()
	}

	existingCond.Status = status
	existingCond.Reason = reason
	existingCond.Message = fmt.Sprintf(message, args...)

	return resList
}

func GetPodListByLabelSelector(ctx context.Context, podLister client.Client, namespace string, selector labels.Selector) (*v1.PodList, error) {
	podList := &v1.PodList{}
	err := podLister.List(ctx, podList, &client.ListOptions{
		Namespace:     namespace,
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to get pods: %v", err)
	}
	return podList, nil
}

func CountReadyPods(podList *v1.PodList) (int64, error) {
	if podList == nil || len(podList.Items) == 0 {
		return 0, nil
	}

	readyPodCount := 0
	for _, pod := range podList.Items {
		isReady := IsPodReady(&pod)
		if pod.Status.Phase == v1.PodRunning && isReady {
			readyPodCount++
		}
		klog.V(4).InfoS("CountReadyPods Pod status", "name", pod.Name, "phase", pod.Status.Phase, "ready", isReady)
	}

	return int64(readyPodCount), nil
}

func FilterReadyPod(pod *v1.Pod) bool {
	return pod.Status.PodIP != "" && !IsPodTerminating(pod) && IsPodReady(pod)
}

// CountRoutablePods filters and returns the number of pods that are routable.
// A pod is routable if it have a valid PodIP and not in terminating state.
func CountRoutablePods(pods []*v1.Pod) (cnt int) {
	for _, pod := range pods {
		if !FilterReadyPod(pod) {
			continue
		}
		cnt++
	}
	return
}

// FilterRoutablePods filters and returns a list of pods that are routable.
// A pod is routable if it have a valid PodIP and not in terminating state.
func FilterRoutablePods(pods []*v1.Pod) []*v1.Pod {
	readyPods := make([]*v1.Pod, 0, len(pods))
	for _, pod := range pods {
		if !FilterReadyPod(pod) {
			continue
		}
		readyPods = append(readyPods, pod)
	}
	return readyPods
}

// FilterRoutablePodsInPlace filters a list of pods that are routable.
// A pod is routable if it have a valid PodIP and not in terminating state.
func FilterRoutablePodsInPlace(pods []*v1.Pod) []*v1.Pod {
	readyCnt := 0
	for i, pod := range pods {
		if !FilterReadyPod(pod) {
			continue
		} else if readyCnt != i {
			pods[readyCnt] = pod
		}
		readyCnt++
	}
	return pods[:readyCnt]
}

// FilterActivePods returns active pods.
func FilterActivePods(pods []v1.Pod) []v1.Pod {
	return FilterPods(pods, FilterReadyPod)
}

type filterPod func(p *v1.Pod) bool

// FilterPods returns replica sets that are filtered by filterFn (all returned ones should match filterFn).
func FilterPods(pods []v1.Pod, filterFn filterPod) []v1.Pod {
	var filtered []v1.Pod
	for i := range pods {
		if filterFn(&pods[i]) {
			filtered = append(filtered, pods[i])
		}
	}
	return filtered
}

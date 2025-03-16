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
	"sort"
	"sync"

	v1 "k8s.io/api/core/v1"
)

const (
	DeploymentIdentifier string = "app.kubernetes.io/name"
)

type PodArray struct {
	Pods []*v1.Pod

	deployments      []string
	podsByDeployment map[string][]*v1.Pod
	mu               sync.Mutex
}

func (arr *PodArray) Len() int {
	return len(arr.Pods)
}

func (arr *PodArray) PodsByDeployments(deploymentName string) []*v1.Pod {
	if len(arr.Pods) == 0 {
		return nil
	}

	if arr.podsByDeployment == nil {
		arr.initDeployments()
	}

	return arr.podsByDeployment[deploymentName]
}

func (arr *PodArray) Deployments() []string {
	if len(arr.Pods) == 0 {
		return nil
	}

	if arr.podsByDeployment == nil {
		arr.initDeployments()
	}

	return arr.deployments
}

func (arr *PodArray) initDeployments() {
	arr.mu.Lock()
	defer arr.mu.Unlock()

	if arr.podsByDeployment != nil {
		return
	}

	// Sort by deploymentName
	podClasses := make(map[string]int)
	podIndexes := make(map[string]int, len(arr.Pods))
	seen := 0
	for _, pod := range arr.Pods {
		deploymentName := pod.Labels[DeploymentIdentifier] // Count "" in.
		idx, ok := podClasses[deploymentName]
		if !ok {
			idx = seen
			podClasses[deploymentName] = seen
			seen++
		}
		podIndexes[pod.Name] = idx
	}
	sort.Slice(arr.Pods, func(i, j int) bool {
		return podIndexes[arr.Pods[i].Name] < podIndexes[arr.Pods[j].Name]
	})

	// Split and map Pods to deployments
	podsByDeployment := make(map[string][]*v1.Pod, len(podClasses))
	deployments := make([]string, 0, len(podClasses))
	offset := 0
	lastClass := podIndexes[arr.Pods[0].Name]
	lastDeploymentName := arr.Pods[0].Labels[DeploymentIdentifier]
	for i, pod := range arr.Pods {
		if podIndexes[pod.Name] != lastClass {
			podsByDeployment[lastDeploymentName] = arr.Pods[offset:i]
			offset = i
			lastClass = podIndexes[pod.Name]
			lastDeploymentName = pod.Labels[DeploymentIdentifier]
			deployments = append(deployments, lastDeploymentName)
		}
	}
	podsByDeployment[lastDeploymentName] = arr.Pods[offset:]
	deployments = append(deployments, lastDeploymentName)

	// Set arr.podsByDeployment at last
	arr.deployments = deployments
	arr.podsByDeployment = podsByDeployment
}

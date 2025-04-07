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
	"fmt"
	"math/rand"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func genPods(cnt int, readyCnt int) []*v1.Pod {
	readyMap := make([]int, cnt) // 0: ready, 1: no ip, 2: unready, 3: terminated
	for i := readyCnt; i < cnt; i++ {
		readyMap[i] = rand.Intn(3) + 1
	}
	// Random permutation
	for i := cnt - 1; i > 0; i-- {
		j := rand.Intn(i + 1)                               // Generate a random index from 0 to i (inclusive)
		readyMap[i], readyMap[j] = readyMap[j], readyMap[i] // Swap elements
	}

	pods := make([]*v1.Pod, 0, cnt)
	for i := 0; i < cnt; i++ {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("p%d", i+1),
			},
			Status: v1.PodStatus{
				PodIP: fmt.Sprintf("10.0.0.%d", i+1),
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			},
		}
		switch readyMap[i] {
		case 1:
			pod.Status.PodIP = ""
		case 2:
			pod.Status.Conditions[0].Status = v1.ConditionFalse
		case 3:
			pod.ObjectMeta.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		}
		pods = append(pods, pod)
	}
	return pods
}

var _ = Describe("Pod", func() {
	It("should FilterRoutablePodsInPlace return sames as FilterRoutablePods", func() {
		original := genPods(100, 75)
		expected := FilterRoutablePods(original)
		Expect(len(expected)).To(Equal(75))

		modified := FilterRoutablePodsInPlace(original)

		Expect(modified[0]).To(BeIdenticalTo(original[0]))
		Expect(cap(modified)).To(Equal(cap(original)))
		Expect(len(modified)).NotTo(Equal(len(original)))

		Expect(modified).To(Equal(expected))
	})

	Describe("DeploymentNameFromPod", func() {

		It("should DeploymentNameFromPod return correct deployment name from pod labels", func() {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						DeploymentIdentifier: "test-deployment",
					},
				},
			}
			expected := "test-deployment"
			result := DeploymentNameFromPod(pod)
			Expect(result).To(Equal(expected))
		})

		It("should DeploymentNameFromPod return correct deployment name from ReplicaSet ownerReferences", func() {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							Kind: "ReplicaSet",
							Name: "mock-llama2-7b-754558b67c",
						},
					},
				},
			}
			expected := "mock-llama2-7b"
			result := DeploymentNameFromPod(pod)
			Expect(result).To(Equal(expected))
		})

		It("should DeploymentNameFromPod return empty string if no valid source found", func() {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels:          map[string]string{},
					OwnerReferences: []metav1.OwnerReference{},
				},
			}
			expected := ""
			result := DeploymentNameFromPod(pod)
			Expect(result).To(Equal(expected))
		})
	})
})

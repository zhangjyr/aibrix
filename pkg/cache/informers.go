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
	"errors"

	crdinformers "github.com/vllm-project/aibrix/pkg/client/informers/externalversions"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	v1alpha1 "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	v1alpha1scheme "github.com/vllm-project/aibrix/pkg/client/clientset/versioned/scheme"
	"k8s.io/client-go/kubernetes/scheme"
)

const (
	modelIdentifier = "model.aibrix.ai/name"
	nodeType        = "ray.io/node-type"
	nodeWorker      = "worker"
)

func initCacheInformers(instance *Store, config *rest.Config, stopCh <-chan struct{}) error {
	if err := v1alpha1scheme.AddToScheme(scheme.Scheme); err != nil {
		return err
	}

	k8sClientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	crdClientSet, err := v1alpha1.NewForConfig(config)
	if err != nil {
		return err
	}

	factory := informers.NewSharedInformerFactoryWithOptions(k8sClientSet, 0)
	crdFactory := crdinformers.NewSharedInformerFactoryWithOptions(crdClientSet, 0)

	podInformer := factory.Core().V1().Pods().Informer()
	modelInformer := crdFactory.Model().V1alpha1().ModelAdapters().Informer()

	defer runtime.HandleCrash()
	factory.Start(stopCh)
	crdFactory.Start(stopCh)

	if !cache.WaitForCacheSync(stopCh, podInformer.HasSynced, modelInformer.HasSynced) {
		return errors.New("timed out waiting for caches to sync")
	}

	if _, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    instance.addPod,
		UpdateFunc: instance.updatePod,
		DeleteFunc: instance.deletePod,
	}); err != nil {
		return err
	}

	if _, err = modelInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    instance.addModelAdapter,
		UpdateFunc: instance.updateModelAdapter,
		DeleteFunc: instance.deleteModelAdapter,
	}); err != nil {
		return err
	}

	return nil
}

func (c *Store) addPod(obj interface{}) {
	pod := obj.(*v1.Pod)
	// only track pods with model deployments
	modelName, ok := pod.Labels[modelIdentifier]
	if !ok {
		// klog.InfoS("ignored pod without model label", "name", pod.Name)
		return
	}
	// ignore worker pods
	nodeType, ok := pod.Labels[nodeType]
	if ok && nodeType == nodeWorker {
		klog.InfoS("ignored ray worker pod", "name", pod.Name)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	metaPod := c.addPodLocked(pod)
	c.addPodAndModelMappingLocked(metaPod, modelName)

	klog.V(4).Infof("POD CREATED: %s/%s", pod.Namespace, pod.Name)
	go c.debugInfo()
}

func (c *Store) updatePod(oldObj interface{}, newObj interface{}) {
	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)

	_, oldOk := oldPod.Labels[modelIdentifier]
	_, existed := c.metaPods.Load(oldPod.Name) // Make sure nothing left.
	newModelName, newOk := newPod.Labels[modelIdentifier]

	if !oldOk && !existed && !newOk {
		// klog.InfoS("ignored pod without model label", "old pod", oldPod.Name, "old pod existence", existed, "new pod", newPod.Name)
		return // No model information to track in either old or new pod
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// TODO: No in place update is handled here

	// Remove old mappings if present, no adapter will be inherited. (Adapters will be rescaned and readded later)
	if oldOk || existed {
		odlMetaPod := c.deletePodLocked(oldPod.Name)
		if odlMetaPod != nil {
			for _, modelName := range odlMetaPod.Models.Array() {
				c.deletePodAndModelMappingLocked(odlMetaPod.Name, modelName, 1)
			}
		}
	}

	// ignore worker pods
	oldNodeType := oldPod.Labels[nodeType]
	newNodeType := newPod.Labels[nodeType]
	if oldNodeType == nodeWorker || newNodeType == nodeWorker {
		klog.InfoS("ignored ray worker pod", "old pod", oldPod.Name, "new pod", newPod.Name)
		return
	}

	// Add new mappings if present
	if newOk {
		metaPod := c.addPodLocked(newPod)
		c.addPodAndModelMappingLocked(metaPod, newModelName)
	}

	klog.V(4).Infof("POD UPDATED: %s/%s %s", newPod.Namespace, newPod.Name, newPod.Status.Phase)
	go c.debugInfo()
}

func (c *Store) deletePod(obj interface{}) {
	pod := obj.(*v1.Pod)
	_, ok := pod.Labels[modelIdentifier]
	_, existed := c.metaPods.Load(pod.Name) // Make sure nothing left.
	if !ok && !existed {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// delete base model and associated lora models on this pod
	metaPod := c.deletePodLocked(pod.Name)
	if metaPod != nil {
		for _, modelName := range metaPod.Models.Array() {
			c.deletePodAndModelMappingLocked(pod.Name, modelName, 1)
		}
	}

	klog.V(4).Infof("POD DELETED: %s/%s", pod.Namespace, pod.Name)
	go c.debugInfo()
}

func (c *Store) addModelAdapter(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	model := obj.(*modelv1alpha1.ModelAdapter)
	for _, pod := range model.Status.Instances {
		c.addPodAndModelMappingLockedByName(pod, model.Name)
	}

	klog.V(4).Infof("MODELADAPTER CREATED: %s/%s", model.Namespace, model.Name)
	go c.debugInfo()
}

func (c *Store) updateModelAdapter(oldObj interface{}, newObj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldModel := oldObj.(*modelv1alpha1.ModelAdapter)
	newModel := newObj.(*modelv1alpha1.ModelAdapter)
	for _, pod := range oldModel.Status.Instances {
		c.deletePodAndModelMappingLocked(pod, oldModel.Name, 0)
	}

	for _, pod := range newModel.Status.Instances {
		c.addPodAndModelMappingLockedByName(pod, newModel.Name)
	}

	klog.V(4).Infof("MODELADAPTER UPDATED. %s/%s %s", oldModel.Namespace, oldModel.Name, newModel.Status.Phase)
	go c.debugInfo()
}

func (c *Store) deleteModelAdapter(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	model := obj.(*modelv1alpha1.ModelAdapter)
	for _, pod := range model.Status.Instances {
		c.deletePodAndModelMappingLocked(pod, model.Name, 0)
	}

	klog.V(4).Infof("MODELADAPTER DELETED: %s/%s", model.Namespace, model.Name)
	go c.debugInfo()
}

func (c *Store) addPodLocked(pod *v1.Pod) *Pod {
	if c.bufferPod == nil {
		c.bufferPod = &Pod{
			Pod:    pod,
			Models: NewRegistry[string](),
		}
	} else {
		c.bufferPod.Pod = pod
	}
	metaPod, loaded := c.metaPods.LoadOrStore(pod.Name, c.bufferPod)
	if !loaded {
		c.bufferPod = nil
	}
	return metaPod
}

func (c *Store) addPodAndModelMappingLockedByName(podName, modelName string) {
	pod, ok := c.metaPods.Load(podName)
	if !ok {
		klog.Errorf("pod %s does not exist in internal-cache", podName)
		return
	}

	c.addPodAndModelMappingLocked(pod, modelName)
}

func (c *Store) addPodAndModelMappingLocked(metaPod *Pod, modelName string) {
	if c.bufferModel == nil {
		c.bufferModel = &Model{
			Pods: NewRegistryWithArrayProvider(func(arr []*v1.Pod) *utils.PodArray { return &utils.PodArray{Pods: arr} }),
		}
	}
	metaModel, loaded := c.metaModels.LoadOrStore(modelName, c.bufferModel)
	if !loaded {
		c.bufferModel = nil
	}

	metaPod.Models.Store(modelName, modelName)
	metaModel.Pods.Store(metaPod.Name, metaPod.Pod)
}

func (c *Store) deletePodLocked(podName string) *Pod {
	metaPod, _ := c.metaPods.LoadAndDelete(podName)
	return metaPod
}

// deletePodAndModelMapping delete mappings between pods and model by specified names.
// If ignoreMapping > 0, podToModel mapping will be ignored.
// If ignoreMapping < 0, modelToPod mapping will be ignored
func (c *Store) deletePodAndModelMappingLocked(podName, modelName string, ignoreMapping int) {
	if ignoreMapping <= 0 {
		if metaPod, ok := c.metaPods.Load(podName); ok {
			metaPod.Models.Delete(modelName)
			// PodToModelMapping entry should only be deleted during pod deleting.
		}
	}

	if ignoreMapping >= 0 {
		if meta, ok := c.metaModels.Load(modelName); ok {
			meta.Pods.Delete(podName)
			if meta.Pods.Len() == 0 {
				c.metaModels.Delete(modelName)
			}
		}
	}
}

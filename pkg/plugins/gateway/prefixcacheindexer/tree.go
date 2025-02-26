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

package prefixcacheindexer

import (
	"sort"
	"sync"
	"time"

	"github.com/vllm-project/aibrix/pkg/utils"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	evictionDuration = 5 * time.Minute // NOTE: hardcoded eviction period
)

type TreeNode struct {
	mu            sync.RWMutex // Add mutex for thread safety
	id            int
	children      map[int]*TreeNode
	parent        *TreeNode
	value         []int
	key           []int
	refCounter    []int
	load          int
	lastAccess    time.Time
	evictedPods   map[int]bool
	cachedPods    map[int]bool
	isLeaf        bool
	contextLength int // total length from root to this node
	depth         int
	modelToPods   map[string]map[string]time.Time // model -> {podName -> lastAccessTime}
}

func (n *TreeNode) GetModelToPods() map[string]map[string]time.Time {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.modelToPods
}

func (n *TreeNode) InitAndUpdateModelPod(model string, podName string, timestamp time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.modelToPods == nil {
		n.modelToPods = make(map[string]map[string]time.Time)
	}
	if n.modelToPods[model] == nil {
		n.modelToPods[model] = make(map[string]time.Time)
	}
	n.modelToPods[model][podName] = timestamp
}

func (n *TreeNode) GetRefCounter() []int {
	return n.refCounter
}

func (n *TreeNode) GetLoad() int {
	return n.load
}

func (n *TreeNode) GetLastAccess() time.Time {
	return n.lastAccess
}

func (n *TreeNode) GetEvictedPods() map[int]bool {
	return n.evictedPods
}

func (n *TreeNode) GetCachedPods() map[int]bool {
	return n.cachedPods
}

func (n *TreeNode) GetParent() *TreeNode {
	return n.parent
}

func (n *TreeNode) GetKey() []int {
	return n.key
}

func (n *TreeNode) GetValue() []int {
	return n.value
}

func (n *TreeNode) NumTokens() int {
	return len(n.value)
}

func (n *TreeNode) ContextLength() int {
	return n.contextLength
}

func (n *TreeNode) GetDepth() int {
	return n.depth
}

func (n *TreeNode) GetID() int {
	return n.id
}

func (n *TreeNode) GetChildren() map[int]*TreeNode {
	return n.children
}

func (n *TreeNode) ResetEvictedPods() {
	n.evictedPods = make(map[int]bool)
}

func (n *TreeNode) ResetCachedPods() {
	n.cachedPods = make(map[int]bool)
}

func (n *TreeNode) ResetRefCounter(numPods int) {
	n.refCounter = make([]int, numPods)
}

func (n *TreeNode) RemovePodsNotInCurrentPodSet(currentPodSet map[string]bool) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	podsChanged := false
	for model, podMap := range n.modelToPods {
		for podName := range podMap {
			if !currentPodSet[podName] {
				delete(podMap, podName)
				podsChanged = true
			}
		}
		if len(podMap) == 0 {
			delete(n.modelToPods, model)
		}
	}
	return podsChanged
}

func (n *TreeNode) HasPodForModel(model, podName string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	pods, ok := n.modelToPods[model]
	if !ok {
		return false
	}
	_, exists := pods[podName]
	return exists
}

func (n *TreeNode) HasValidPods(currentPodSet map[string]bool) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	for _, podMap := range n.modelToPods {
		for podName := range podMap {
			if currentPodSet[podName] {
				return true
			}
		}
	}
	return false
}

func (n *TreeNode) GetModelToPodCount() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.modelToPods)
}

func (n *TreeNode) GetPodsForModel(model string) map[string]time.Time {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if _, exists := n.modelToPods[model]; exists {
		return n.modelToPods[model] // Still returns direct reference but smaller scope
	}
	return nil
}

func (n *TreeNode) AddOrUpdatePodForModel(model string, podName string, timestamp time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.modelToPods == nil {
		n.modelToPods = make(map[string]map[string]time.Time)
	}
	if _, exists := n.modelToPods[model]; !exists {
		n.modelToPods[model] = make(map[string]time.Time)
	}
	n.modelToPods[model][podName] = timestamp
}

func (n *TreeNode) RemovePodsNotInSet(currentPodSet map[string]bool) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	podsChanged := false
	for model, podMap := range n.modelToPods {
		for podName := range podMap {
			if !currentPodSet[podName] {
				delete(podMap, podName)
				podsChanged = true
			}
		}
		if len(podMap) == 0 {
			delete(n.modelToPods, model)
		}
	}
	return podsChanged
}

func (c *LPRadixCache) NewTreeNode(numPods int, parent *TreeNode, key []int, value []int) *TreeNode {
	// Create the node with initialized maps and slices
	node := &TreeNode{
		id:          c.nextNodeID,
		children:    make(map[int]*TreeNode),
		parent:      parent,
		key:         make([]int, len(key)),   // Allocate space for key
		value:       make([]int, len(value)), // Allocate space for value (using len(value), not len(key))
		refCounter:  make([]int, numPods),
		load:        1,
		lastAccess:  time.Now(),
		evictedPods: make(map[int]bool),
		cachedPods:  make(map[int]bool),
		modelToPods: make(map[string]map[string]time.Time),
		// Pods:          make(map[string]time.Time),
		depth:         0,
		contextLength: 0,
	}

	// Increment node ID for next creation
	klog.Infof("Created a new node(%d) with key: %v and value: %v", node.id, key, value)
	c.nextNodeID++

	// Set depth and context length based on parent
	if parent != nil {
		node.depth = parent.depth + 1
		node.contextLength = parent.contextLength + len(key)
	}

	// Copy key and value slices
	if len(key) > 0 {
		copy(node.key, key)
	}
	if len(value) > 0 {
		copy(node.value, value)
	}

	return node
}

func (c *LPRadixCache) PrettyPrint() {
	// c.mu.RLock()
	// defer c.mu.RUnlock()
	c.prettyPrintHelper(c.rootNode, "", true)
}

func (c *LPRadixCache) GetAllNodes() map[int]*TreeNode {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.allNodes
}

func (c *LPRadixCache) GetAllPodsInNode(node *TreeNode) []string {
	node.mu.RLock()
	defer node.mu.RUnlock()
	all_pods_in_node := make([]string, 0)
	for _, pods := range node.modelToPods {
		for podName := range pods {
			all_pods_in_node = append(all_pods_in_node, podName)
		}
	}
	return all_pods_in_node
}

func (c *LPRadixCache) prettyPrintHelper(node *TreeNode, prefix string, isLast bool) {
	if node == nil {
		return
	}
	marker := "└── "
	if !isLast {
		marker = "├── "
	}
	childPrefix := prefix + "    "
	if !isLast {
		childPrefix = prefix + "│   "
	}
	// klog.Infof("%s%s[Key: %v, Value: %v, Load: %d, Depth: %d]", prefix, marker, node.key, node.value, node.load, node.depth)
	tokens_in_string, err := utils.DetokenizeText(node.key)
	if err != nil {
		klog.Errorf("Failed to detokenize key for node %d: %v", node.id, err)
		tokens_in_string = "ERROR"
	}
	all_pods_in_node := c.GetAllPodsInNode(node)
	klog.Infof("%s%s[Node: %d, Tokens: '%s', Load: %d, Pods: %v, Depth: %d]", prefix, marker, node.id, tokens_in_string, node.load, all_pods_in_node, node.depth)
	// if len(node.ModelToPods) > 0 {
	// 	klog.Infof("%s    Models:", prefix)
	// 	for model, pods := range node.ModelToPods {
	// 		podNames := make([]string, 0, len(pods))
	// 		for podName := range pods {
	// 			podNames = append(podNames, podName)
	// 		}
	// 		klog.Infof("%s    └── %s: %v", prefix, model, podNames)
	// 	}
	// }
	childKeys := make([]int, 0, len(node.children))
	for k := range node.children {
		childKeys = append(childKeys, k)
	}
	sort.Ints(childKeys)

	for i, key := range childKeys {
		isLastChild := i == len(childKeys)-1
		c.prettyPrintHelper(node.children[key], childPrefix, isLastChild)
	}
}

type LPRadixCache struct {
	mu       sync.RWMutex
	rootNode *TreeNode
	numPods  int
	// allocatedSize []int // not being used. if it is not going to be used, it will be removed permanently.
	allNodes   map[int]*TreeNode
	nextNodeID int
	startTime  time.Time
}

func NewLPRadixCache(numPods int) *LPRadixCache {
	cache := &LPRadixCache{
		numPods: numPods,
		// allocatedSize: make([]int, numPods), // not being used. if it is not going to be used, it will be removed permanently.
		allNodes:   make(map[int]*TreeNode),
		nextNodeID: 0,
		startTime:  time.Now(),
	}
	cache.reset()
	return cache
}

func (c *LPRadixCache) reset() {
	root := c.NewTreeNode(c.numPods, nil, []int{}, []int{})
	for i := range root.refCounter {
		root.refCounter[i] = 1
	}
	c.rootNode = root
	c.allNodes = make(map[int]*TreeNode)
	c.allNodes[root.id] = root
}

// matchLen returns the length of matching prefix between two slices
func matchLen(key, seq []int) int {
	i := 0
	for i < len(key) && i < len(seq) {
		if key[i] != seq[i] {
			break
		}
		i++
	}
	return i
}

// Add internal method to get node
func (c *LPRadixCache) GetNode(tokens []int) *TreeNode {
	c.mu.RLock()
	defer c.mu.RUnlock()
	node, _ := c.matchPrefixHelper(c.rootNode, tokens)
	return node
}

// Implementation of PrefixCacheIndexer interface
// NOTE: It is currently Not being used. Everything is being done in AddPrefix interface.
func (c *LPRadixCache) MatchPrefix(inputTokens []int, model string, pods []*v1.Pod) ([]int, []int, []*v1.Pod) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	node, matchedTokens := c.matchPrefixHelper(c.rootNode, inputTokens)
	if node == nil || len(matchedTokens) == 0 {
		return []int{}, inputTokens, nil
	}

	unmatchedTokens := inputTokens[len(matchedTokens):]

	// Find matching pods
	var matchedPods []*v1.Pod
	if modelPods, ok := node.modelToPods[model]; ok {
		for _, pod := range pods {
			if _, ok := modelPods[pod.Name]; ok {
				if matchedPods == nil {
					matchedPods = make([]*v1.Pod, 0, len(pods))
				}
				matchedPods = append(matchedPods, pod)
				klog.Infof("Matched pod for node(%d): %s", node.id, pod.Name)
			}
		}
	}

	klog.Infof("MatchPrefix - node(%d) key: %v, matched tokens: %v, model pods: %v",
		node.id, node.key, matchedTokens, node.modelToPods)

	return matchedTokens, unmatchedTokens, matchedPods
}

// This is being used by GetNode
func (c *LPRadixCache) matchPrefixHelper(node *TreeNode, tokens []int) (*TreeNode, []int) {
	if len(tokens) == 0 {
		return node, nil
	}

	node.lastAccess = time.Now()
	if child, ok := node.children[tokens[0]]; ok {
		prefixLen := matchLen(child.key, tokens)
		if prefixLen > 0 {
			if prefixLen == len(child.key) {
				// Complete match with this node's key
				if prefixLen == len(tokens) {
					return child, child.key
				}
				// Continue matching with remaining tokens
				deeperNode, deeperMatched := c.matchPrefixHelper(child, tokens[prefixLen:])
				if deeperNode != nil && len(deeperMatched) > 0 {
					return deeperNode, append(child.key, deeperMatched...)
				}
				return child, child.key
			}
			// Partial match with this node's key
			return child, child.key[:prefixLen]
		}
	}
	return node, nil
}

func (c *LPRadixCache) AddPrefix(tokens []int, model string, podName string) (*TreeNode, []int, []int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Do insertion first
	node, matchedTokens, unmatchedTokens := c.insertHelper(c.rootNode, tokens, tokens)
	if node != nil {
		node.InitAndUpdateModelPod(model, podName, time.Now())
		current := node
		for current.parent != nil {
			current.parent.InitAndUpdateModelPod(model, podName, time.Now())
			current = current.parent
		}
		klog.Infof("Updated mapping for model %s, pod %s in node(%d) with key %v",
			model, podName, node.id, node.key)
	}
	// c.PrettyPrint()
	return node, matchedTokens, unmatchedTokens
}

func (c *LPRadixCache) insertHelper(node *TreeNode, key []int, value []int) (*TreeNode, []int, []int) {
	node.lastAccess = time.Now()
	node.load++
	klog.Infof("Trying to insert key: %v into node(%d)", key, node.id)
	timePassed := node.lastAccess.Sub(c.startTime).Seconds()
	klog.Infof("Updated node(%d) last access: %.2f seconds", node.id, timePassed)

	if len(key) == 0 {
		return node, nil, nil
	}

	// Check if one of the children matches the prefix
	if child, ok := node.children[key[0]]; ok {
		prefixLen := matchLen(child.key, key)

		// Case 1: Complete match with child's key
		if prefixLen == len(child.key) {
			if prefixLen == len(key) {
				klog.Infof("Entire input tokens match the child node(%d): %v", child.id, key)
				child.lastAccess = time.Now()
				child.load++
				return child, key, nil // Return the original key for exact match
			}
			// Partial match, continue deeper
			klog.Infof("Partial tokens match child node(%d): %v. Continue deeper", child.id, key)
			childNode, childMatched, childUnmatched := c.insertHelper(child, key[prefixLen:], value[prefixLen:])
			if len(childMatched) > 0 {
				return childNode, key[:prefixLen+len(childMatched)], childUnmatched
			}
			return childNode, key[:prefixLen], key[prefixLen:]
		}

		// Case 2: Partial match, need to split
		newNode := c.splitNode(key, child, prefixLen)
		if prefixLen == len(key) {
			return newNode, key, nil
		}
		deeperNode, deeperMatched, deeperUnmatched := c.insertHelper(newNode, key[prefixLen:], value[prefixLen:])
		if len(deeperMatched) > 0 {
			return deeperNode, key[:prefixLen+len(deeperMatched)], deeperUnmatched
		}
		return deeperNode, key[:prefixLen], key[prefixLen:]
	}

	// No matching child, create new node
	klog.Info("No child matches any of the prefix: ", key)
	newNode := c.NewTreeNode(c.numPods, node, key, value)
	node.children[key[0]] = newNode
	c.allNodes[newNode.id] = newNode
	return newNode, nil, key
}

func (c *LPRadixCache) doesExceededTTL(node *TreeNode, now time.Time) bool {
	timeSinceLastAccess := now.Sub(node.lastAccess)
	if timeSinceLastAccess > evictionDuration {
		klog.Infof("Node(%d) exceeded TTL(%ds), time since last access: %.2f seconds",
			node.id, int(evictionDuration.Seconds()), timeSinceLastAccess.Seconds())
		return true
	}
	return false
}

func (c *LPRadixCache) Evict(now time.Time) []*TreeNode {
	c.mu.Lock()
	defer c.mu.Unlock()
	var nodesToEvict []*TreeNode
	for _, node := range c.allNodes {
		if node != c.rootNode {
			if c.doesExceededTTL(node, now) {
				// timePassed := now.Sub(node.lastAccess).Seconds()
				// klog.Infof("Node(%d) exceeded TTL(%ds), time since last access: %.2f",
				// 	node.id, int(evictionDuration.Seconds()), timePassed)
				if collected := c.collectNodeAndChildren(node); collected != nil {
					nodesToEvict = append(nodesToEvict, collected...)
				}
			}
		}
	}
	// Actually perform the eviction
	for _, node := range nodesToEvict {
		c.evictNode(node)
	}
	if len(nodesToEvict) > 0 {
		klog.Infof("Evicted %d nodes", len(nodesToEvict))
		c.PrettyPrint()
	}
	return nodesToEvict
}

func (c *LPRadixCache) collectNodeAndChildren(node *TreeNode) []*TreeNode {
	if node == c.rootNode {
		return nil
	}
	nodes := make([]*TreeNode, 0)
	stack := []*TreeNode{node}
	// BFS
	for len(stack) > 0 {
		current := stack[len(stack)-1] // top
		stack = stack[:len(stack)-1]   // pop
		nodes = append(nodes, current) // collect
		for _, child := range current.children {
			stack = append(stack, child)
		}
	}
	return nodes
}

// Fix for evictNode method in tree.go
func (c *LPRadixCache) evictNode(node *TreeNode) {
	if node == c.rootNode {
		return
	}

	// Clean up pod mappings in parent nodes
	current := node
	for parent := node.parent; parent != nil; parent = parent.parent {
		// Remove this node's pod mappings from parent
		for model, pods := range current.modelToPods {
			if parentPods, ok := parent.modelToPods[model]; ok {
				for podName := range pods {
					delete(parentPods, podName)
				}
				// Remove model mapping if no pods left
				if len(parentPods) == 0 {
					delete(parent.modelToPods, model)
				}
			}
		}
	}

	// Remove node from parent's children
	if node.parent != nil {
		delete(node.parent.children, node.key[0])
	}

	// Remove from allNodes map
	delete(c.allNodes, node.id)
	klog.Infof("Evict node(%d)!, Key: %v", node.id, node.key)

	// Clean up the node's references
	node.parent = nil
	node.children = nil
	node.modelToPods = nil
	node.evictedPods = nil
	node.cachedPods = nil
	node.value = nil
	node.key = nil
	node.refCounter = nil
}

func (c *LPRadixCache) splitNode(key []int, child *TreeNode, splitLen int) *TreeNode {
	klog.Infof("Splitting node(%d): %v, into %v and %v", child.id, child.key, child.key[:splitLen], child.key[splitLen:])

	// Create new node with split portions
	newNode := c.NewTreeNode(c.numPods, child.parent, child.key[:splitLen], child.value[:splitLen])

	// Update parent's reference to point to new node
	child.parent.children[key[0]] = newNode

	// Update child node
	remainingKey := make([]int, len(child.key)-splitLen)
	copy(remainingKey, child.key[splitLen:])
	child.key = remainingKey

	remainingValue := make([]int, len(child.value)-splitLen)
	copy(remainingValue, child.value[splitLen:])
	child.value = remainingValue

	// Update relationships
	child.parent = newNode
	newNode.children = make(map[int]*TreeNode)
	if len(child.key) > 0 {
		newNode.children[child.key[0]] = child
	}

	// Copy metadata
	newNode.load = child.load
	copy(newNode.refCounter, child.refCounter)

	// Copy pod mappings
	for k, v := range child.cachedPods {
		newNode.cachedPods[k] = v
	}
	for k, v := range child.evictedPods {
		newNode.evictedPods[k] = v
	}

	// Copy ModelToPods mapping to both nodes
	newNode.modelToPods = make(map[string]map[string]time.Time)
	for model, pods := range child.modelToPods {
		// Copy to new node (prefix node)
		newNode.modelToPods[model] = make(map[string]time.Time)
		for podName, lastAccess := range pods {
			newNode.modelToPods[model][podName] = lastAccess
		}
	}

	klog.Infof("Split complete - New node(%d) key: %v, modelToPods: %v",
		newNode.id, newNode.key, newNode.modelToPods)
	klog.Infof("Split complete - Child node(%d) key: %v, modelToPods: %v",
		child.id, child.key, child.modelToPods)

	c.allNodes[newNode.id] = newNode
	return newNode
}

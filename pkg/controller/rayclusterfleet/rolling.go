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

package rayclusterfleet

import (
	"context"
	"fmt"
	"sort"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/rayclusterfleet/util"
	"k8s.io/klog/v2"
)

// rolloutRolling implements the logic for rolling a new replica set.
func (r *RayClusterFleetReconciler) rolloutRolling(ctx context.Context, d *orchestrationv1alpha1.RayClusterFleet, rsList []*orchestrationv1alpha1.RayClusterReplicaSet) error {
	newRS, oldRSs, err := r.getAllReplicaSetsAndSyncRevision(ctx, d, rsList, true)
	if err != nil {
		return err
	}
	allRSs := append(oldRSs, newRS)

	// Scale up, if we can.
	scaledUp, err := r.reconcileNewReplicaSet(ctx, allRSs, newRS, d)
	if err != nil {
		return err
	}
	if scaledUp {
		// Update DeploymentStatus
		return r.syncRolloutStatus(ctx, allRSs, newRS, d)
	}

	// Scale down, if we can.
	scaledDown, err := r.reconcileOldReplicaSets(ctx, allRSs, util.FilterActiveReplicaSets(oldRSs), newRS, d)
	if err != nil {
		return err
	}
	if scaledDown {
		// Update DeploymentStatus
		return r.syncRolloutStatus(ctx, allRSs, newRS, d)
	}

	if util.DeploymentComplete(d, &d.Status) {
		if err := r.cleanupDeployment(ctx, oldRSs, d); err != nil {
			return err
		}
	}

	// Sync deployment status
	return r.syncRolloutStatus(ctx, allRSs, newRS, d)
}

func (r *RayClusterFleetReconciler) reconcileNewReplicaSet(ctx context.Context, allRSs []*orchestrationv1alpha1.RayClusterReplicaSet, newRS *orchestrationv1alpha1.RayClusterReplicaSet, deployment *orchestrationv1alpha1.RayClusterFleet) (bool, error) {
	if *(newRS.Spec.Replicas) == *(deployment.Spec.Replicas) {
		// Scaling not required.
		return false, nil
	}
	if *(newRS.Spec.Replicas) > *(deployment.Spec.Replicas) {
		// Scale down.
		scaled, _, err := r.scaleReplicaSetAndRecordEvent(ctx, newRS, *(deployment.Spec.Replicas), deployment)
		return scaled, err
	}
	newReplicasCount, err := util.NewRSNewReplicas(deployment, allRSs, newRS)
	if err != nil {
		return false, err
	}
	scaled, _, err := r.scaleReplicaSetAndRecordEvent(ctx, newRS, newReplicasCount, deployment)
	return scaled, err
}

func (r *RayClusterFleetReconciler) reconcileOldReplicaSets(ctx context.Context, allRSs []*orchestrationv1alpha1.RayClusterReplicaSet, oldRSs []*orchestrationv1alpha1.RayClusterReplicaSet, newRS *orchestrationv1alpha1.RayClusterReplicaSet, deployment *orchestrationv1alpha1.RayClusterFleet) (bool, error) {
	logger := klog.FromContext(ctx)
	oldPodsCount := util.GetReplicaCountForReplicaSets(oldRSs)
	if oldPodsCount == 0 {
		// Can't scale down further
		return false, nil
	}
	allPodsCount := util.GetReplicaCountForReplicaSets(allRSs)
	logger.V(4).Info("New replica set", "replicaSet", klog.KObj(newRS), "availableReplicas", newRS.Status.AvailableReplicas)
	maxUnavailable := util.MaxUnavailable(*deployment)

	// Check if we can scale down. We can scale down in the following 2 cases:
	// * Some old replica sets have unhealthy replicas, we could safely scale down those unhealthy replicas since that won't further
	//  increase unavailability.
	// * New replica set has scaled up and it's replicas becomes ready, then we can scale down old replica sets in a further step.
	//
	// maxScaledDown := allPodsCount - minAvailable - newReplicaSetPodsUnavailable
	// take into account not only maxUnavailable and any surge pods that have been created, but also unavailable pods from
	// the newRS, so that the unavailable pods from the newRS would not make us scale down old replica sets in a further
	// step(that will increase unavailability).
	//
	// Concrete example:
	//
	// * 10 replicas
	// * 2 maxUnavailable (absolute number, not percent)
	// * 3 maxSurge (absolute number, not percent)
	//
	// case 1:
	// * Deployment is updated, newRS is created with 3 replicas, oldRS is scaled down to 8, and newRS is scaled up to 5.
	// * The new replica set pods crashloop and never become available.
	// * allPodsCount is 13. minAvailable is 8. newRSPodsUnavailable is 5.
	// * A node fails and causes one of the oldRS pods to become unavailable. However, 13 - 8 - 5 = 0, so the oldRS won't be scaled down.
	// * The user notices the crashloop and does kubectl rollout undo to rollback.
	// * newRSPodsUnavailable is 1, since we rolled back to the good replica set, so maxScaledDown = 13 - 8 - 1 = 4. 4 of the crashlooping pods will be scaled down.
	// * The total number of pods will then be 9 and the newRS can be scaled up to 10.
	//
	// case 2:
	// Same example, but pushing a new pod template instead of rolling back (aka "roll over"):
	// * The new replica set created must start with 0 replicas because allPodsCount is already at 13.
	// * However, newRSPodsUnavailable would also be 0, so the 2 old replica sets could be scaled down by 5 (13 - 8 - 0), which would then
	// allow the new replica set to be scaled up by 5.
	minAvailable := *(deployment.Spec.Replicas) - maxUnavailable
	newRSUnavailablePodCount := *(newRS.Spec.Replicas) - newRS.Status.AvailableReplicas
	maxScaledDown := allPodsCount - minAvailable - newRSUnavailablePodCount
	if maxScaledDown <= 0 {
		return false, nil
	}

	// Clean up unhealthy replicas first, otherwise unhealthy replicas will block deployment
	// and cause timeout. See https://github.com/kubernetes/kubernetes/issues/16737
	oldRSs, cleanupCount, err := r.cleanupUnhealthyReplicas(ctx, oldRSs, deployment, maxScaledDown)
	if err != nil {
		return false, nil
	}
	logger.V(4).Info("Cleaned up unhealthy replicas from old RSes", "count", cleanupCount)

	// Scale down old replica sets, need check maxUnavailable to ensure we can scale down
	allRSs = append(oldRSs, newRS)
	scaledDownCount, err := r.scaleDownOldReplicaSetsForRollingUpdate(ctx, allRSs, oldRSs, deployment)
	if err != nil {
		return false, nil
	}
	logger.V(4).Info("Scaled down old RSes", "deployment", klog.KObj(deployment), "count", scaledDownCount)

	totalScaledDown := cleanupCount + scaledDownCount
	return totalScaledDown > 0, nil
}

// cleanupUnhealthyReplicas will scale down old replica sets with unhealthy replicas, so that all unhealthy replicas will be deleted.
func (r *RayClusterFleetReconciler) cleanupUnhealthyReplicas(ctx context.Context, oldRSs []*orchestrationv1alpha1.RayClusterReplicaSet, deployment *orchestrationv1alpha1.RayClusterFleet, maxCleanupCount int32) ([]*orchestrationv1alpha1.RayClusterReplicaSet, int32, error) {
	logger := klog.FromContext(ctx)
	sort.Sort(util.ReplicaSetsByCreationTimestamp(oldRSs))
	// Safely scale down all old replica sets with unhealthy replicas. Replica set will sort the pods in the order
	// such that not-ready < ready, unscheduled < scheduled, and pending < running. This ensures that unhealthy replicas will
	// been deleted first and won't increase unavailability.
	totalScaledDown := int32(0)
	for i, targetRS := range oldRSs {
		if totalScaledDown >= maxCleanupCount {
			break
		}
		if *(targetRS.Spec.Replicas) == 0 {
			// cannot scale down this replica set.
			continue
		}
		logger.V(4).Info("Found available pods in old RS", "replicaSet", klog.KObj(targetRS), "availableReplicas", targetRS.Status.AvailableReplicas)
		if *(targetRS.Spec.Replicas) == targetRS.Status.AvailableReplicas {
			// no unhealthy replicas found, no scaling required.
			continue
		}

		scaledDownCount := min(maxCleanupCount-totalScaledDown, *(targetRS.Spec.Replicas)-targetRS.Status.AvailableReplicas)
		newReplicasCount := *(targetRS.Spec.Replicas) - scaledDownCount
		if newReplicasCount > *(targetRS.Spec.Replicas) {
			return nil, 0, fmt.Errorf("when cleaning up unhealthy replicas, got invalid request to scale down %s/%s %d -> %d", targetRS.Namespace, targetRS.Name, *(targetRS.Spec.Replicas), newReplicasCount)
		}
		_, updatedOldRS, err := r.scaleReplicaSetAndRecordEvent(ctx, targetRS, newReplicasCount, deployment)
		if err != nil {
			return nil, totalScaledDown, err
		}
		totalScaledDown += scaledDownCount
		oldRSs[i] = updatedOldRS
	}
	return oldRSs, totalScaledDown, nil
}

// scaleDownOldReplicaSetsForRollingUpdate scales down old replica sets when deployment strategy is "RollingUpdate".
// Need check maxUnavailable to ensure availability
func (r *RayClusterFleetReconciler) scaleDownOldReplicaSetsForRollingUpdate(ctx context.Context, allRSs []*orchestrationv1alpha1.RayClusterReplicaSet, oldRSs []*orchestrationv1alpha1.RayClusterReplicaSet, deployment *orchestrationv1alpha1.RayClusterFleet) (int32, error) {
	logger := klog.FromContext(ctx)
	maxUnavailable := util.MaxUnavailable(*deployment)

	// Check if we can scale down.
	minAvailable := *(deployment.Spec.Replicas) - maxUnavailable
	// Find the number of available pods.
	availablePodCount := util.GetAvailableReplicaCountForReplicaSets(allRSs)
	if availablePodCount <= minAvailable {
		// Cannot scale down.
		return 0, nil
	}
	logger.V(4).Info("Found available pods in deployment, scaling down old RSes", "deployment", klog.KObj(deployment), "availableReplicas", availablePodCount)

	sort.Sort(util.ReplicaSetsByCreationTimestamp(oldRSs))

	totalScaledDown := int32(0)
	totalScaleDownCount := availablePodCount - minAvailable
	for _, targetRS := range oldRSs {
		if totalScaledDown >= totalScaleDownCount {
			// No further scaling required.
			break
		}
		if *(targetRS.Spec.Replicas) == 0 {
			// cannot scale down this ReplicaSet.
			continue
		}
		// Scale down.
		scaleDownCount := min(*(targetRS.Spec.Replicas), totalScaleDownCount-totalScaledDown)
		newReplicasCount := *(targetRS.Spec.Replicas) - scaleDownCount
		if newReplicasCount > *(targetRS.Spec.Replicas) {
			return 0, fmt.Errorf("when scaling down old RS, got invalid request to scale down %s/%s %d -> %d", targetRS.Namespace, targetRS.Name, *(targetRS.Spec.Replicas), newReplicasCount)
		}
		_, _, err := r.scaleReplicaSetAndRecordEvent(ctx, targetRS, newReplicasCount, deployment)
		if err != nil {
			return totalScaledDown, err
		}

		totalScaledDown += scaleDownCount
	}

	return totalScaledDown, nil
}

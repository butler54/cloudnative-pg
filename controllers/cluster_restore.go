/*
Copyright The CloudNativePG Contributors

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

package controllers

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/management/log"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/specs"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
)

// reconcileRestoredCluster ensures that we own again any orphan resources when cluster gets reconciled for
// the first time
func (r *ClusterReconciler) reconcileRestoredCluster(ctx context.Context, cluster *apiv1.Cluster) error {
	contextLogger := log.FromContext(ctx)

	// No need to check this on a cluster which has been already deployed
	if cluster.Status.LatestGeneratedNode != 0 {
		return nil
	}

	// Get the list of PVCs belonging to this cluster but not owned by it
	pvcs, err := getOrphanPVCs(ctx, r.Client, cluster)
	if err != nil {
		return err
	}
	if len(pvcs) == 0 {
		contextLogger.Info("no orphan PVCs found, skipping the restored cluster reconciliation")
		return nil
	}

	contextLogger.Info("found orphan pvcs, trying to restore the cluster", "pvcs", pvcs)

	highestSerial, primarySerial, err := getNodeSerialsFromPVCs(pvcs)
	if err != nil {
		return err
	}

	if primarySerial == 0 {
		contextLogger.Info("no primary serial found, assigning the highest serial as the primary")
		primarySerial = highestSerial
	}

	contextLogger.Debug("proceeding to restore the cluster status")
	if err := restoreClusterStatus(ctx, r.Client, cluster, highestSerial, primarySerial); err != nil {
		return err
	}

	contextLogger.Debug("restored the cluster status, proceeding to restore the orphan PVCS")
	return restoreOrphanPVCs(ctx, r.Client, cluster, pvcs)
}

// restoreClusterStatus bootstraps the status needed to make the restored cluster work
func restoreClusterStatus(
	ctx context.Context,
	c client.Client,
	cluster *apiv1.Cluster,
	latestNodeSerial int,
	targetPrimaryNodeSerial int,
) error {
	clusterOrig := cluster.DeepCopy()
	cluster.Status.LatestGeneratedNode = latestNodeSerial
	cluster.Status.TargetPrimary = specs.GetInstanceName(cluster.Name, targetPrimaryNodeSerial)
	return c.Status().Patch(ctx, cluster, client.MergeFrom(clusterOrig))
}

func getOrphanPVCs(
	ctx context.Context,
	c client.Client,
	cluster *apiv1.Cluster,
) ([]corev1.PersistentVolumeClaim, error) {
	contextLogger := log.FromContext(ctx).WithValues("step", "get_orphan_pvcs")

	var pvcList corev1.PersistentVolumeClaimList
	if err := c.List(
		ctx,
		&pvcList,
		client.MatchingLabels{utils.ClusterLabelName: cluster.Name},
	); err != nil {
		return nil, err
	}

	orphanPVCs := make([]corev1.PersistentVolumeClaim, 0, len(pvcList.Items))
	for _, pvc := range pvcList.Items {
		if len(pvc.OwnerReferences) != 0 {
			contextLogger.Warning("skipping pvc because it has owner metadata",
				"pvcName", pvc.Name)
			continue
		}
		if _, ok := pvc.Annotations[specs.ClusterSerialAnnotationName]; !ok {
			contextLogger.Warning("skipping pvc because it doesn't have serial annotation",
				"pvcName", pvc.Name)
			continue
		}

		orphanPVCs = append(orphanPVCs, pvc)
	}

	return orphanPVCs, nil
}

// getNodeSerialsFromPVCs tries to obtain the highestSerial and the primary serial from a group of PVCs
func getNodeSerialsFromPVCs(
	pvcs []corev1.PersistentVolumeClaim,
) (int, int, error) {
	var highestSerial int
	var primarySerial int
	for _, pvc := range pvcs {
		serial, err := specs.GetNodeSerial(pvc.ObjectMeta)
		if err != nil {
			return 0, 0, err
		}
		if serial > highestSerial {
			highestSerial = serial
		}
		if pvc.ObjectMeta.Labels[specs.ClusterRoleLabelName] == specs.ClusterRoleLabelPrimary {
			primarySerial = serial
		}
	}

	return highestSerial, primarySerial, nil
}

// restoreOrphanPVCs sets the owner metadata and re-actives the orphan pvcs
func restoreOrphanPVCs(
	ctx context.Context,
	c client.Client,
	cluster *apiv1.Cluster,
	pvcs []corev1.PersistentVolumeClaim,
) error {
	for i := range pvcs {
		pvc := &pvcs[i]
		if pvc.Annotations == nil {
			pvc.Annotations = map[string]string{}
		}

		pvcOrig := pvc.DeepCopy()
		SetClusterOwnerAnnotationsAndLabels(&pvc.ObjectMeta, cluster)
		pvc.Annotations[specs.PVCStatusAnnotationName] = specs.PVCStatusReady
		// we clean hibernation metadata if it exists
		delete(pvc.Annotations, utils.HibernateClusterManifestAnnotationName)
		delete(pvc.Annotations, utils.HibernatePgControlDataAnnotationName)

		if err := c.Patch(ctx, pvc, client.MergeFrom(pvcOrig)); err != nil {
			return err
		}
	}

	return nil
}

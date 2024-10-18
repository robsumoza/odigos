package gateway

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"

	odigosv1 "github.com/odigos-io/odigos/api/odigos/v1alpha1"
	commonconf "github.com/odigos-io/odigos/autoscaler/controllers/common"
	odigoscommon "github.com/odigos-io/odigos/common"
	k8sconsts "github.com/odigos-io/odigos/k8sutils/pkg/consts"
	"github.com/odigos-io/odigos/k8sutils/pkg/env"
	"github.com/odigos-io/odigos/k8sutils/pkg/utils"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	ClusterCollectorGateway = map[string]string{
		k8sconsts.OdigosCollectorRoleLabel: string(k8sconsts.CollectorsRoleClusterGateway),
	}
)

func Sync(ctx context.Context, k8sClient client.Client, scheme *runtime.Scheme, imagePullSecrets []string, odigosVersion string) error {
	logger := log.FromContext(ctx)

	odigosNs := env.GetCurrentNamespace()
	var gatewayCollectorGroup odigosv1.CollectorsGroup
	err := k8sClient.Get(ctx, client.ObjectKey{Namespace: odigosNs, Name: k8sconsts.OdigosClusterCollectorConfigMapName}, &gatewayCollectorGroup)
	if err != nil {
		// collectors group is created by the scheduler, after the first destination is added.
		// it is however possible that some reconciler (like deployment) triggered and the collectors group will be created shortly.
		return client.IgnoreNotFound(err)
	}

	var dests odigosv1.DestinationList
	if err := k8sClient.List(ctx, &dests); err != nil {
		logger.Error(err, "Failed to list destinations")
		return err
	}

	var processors odigosv1.ProcessorList
	if err := k8sClient.List(ctx, &processors); err != nil {
		logger.Error(err, "Failed to list processors")
		return err
	}
	// Add the generic batch processor to the list of processors
	processors.Items = append(processors.Items, commonconf.GetGenericBatchProcessor())

	odigosConfig, err := utils.GetCurrentOdigosConfig(ctx, k8sClient)
	if err != nil {
		logger.Error(err, "failed to get odigos config")
		return err
	}

	err = syncGateway(&dests, &processors, &gatewayCollectorGroup, ctx, k8sClient, scheme, imagePullSecrets, odigosVersion, &odigosConfig)
	statusPatchString := commonconf.GetCollectorsGroupDeployedConditionsPatch(err)
	statusErr := k8sClient.Status().Patch(ctx, &gatewayCollectorGroup, client.RawPatch(types.MergePatchType, []byte(statusPatchString)))
	if statusErr != nil {
		logger.Error(statusErr, "Failed to patch collectors group status")
		// just log the error, do not fail the reconciliation
	}
	return err
}

func syncGateway(dests *odigosv1.DestinationList, processors *odigosv1.ProcessorList,
	gateway *odigosv1.CollectorsGroup, ctx context.Context,
	c client.Client, scheme *runtime.Scheme, imagePullSecrets []string, odigosVersion string, odigosConfig *odigoscommon.OdigosConfiguration) error {
	logger := log.FromContext(ctx)
	logger.V(0).Info("Syncing gateway")

	memConfig := getMemoryConfigurations(odigosConfig)

	configData, signals, err := syncConfigMap(dests, processors, gateway, ctx, c, scheme, memConfig)
	if err != nil {
		logger.Error(err, "Failed to sync config map")
		return err
	}

	err = deletePreviousServices(ctx, c, gateway.Namespace)
	if err != nil {
		logger.Error(err, "Failed to delete previous services")
		return err
	}

	_, err = syncService(gateway, ctx, c, scheme)
	if err != nil {
		logger.Error(err, "Failed to sync service")
		return err
	}

	_, err = syncDeployment(dests, gateway, configData, ctx, c, scheme, imagePullSecrets, odigosVersion, memConfig)
	if err != nil {
		logger.Error(err, "Failed to sync deployment")
		return err
	}

	err = commonconf.UpdateCollectorGroupReceiverSignals(ctx, c, gateway, signals)
	if err != nil {
		logger.Error(err, "Failed to update cluster collectors group received signals")
		return err
	}

	if isMetricsServerInstalled(ctx, c) {
		err = syncHPA(gateway, ctx, c, scheme, memConfig)
		if err != nil {
			logger.Error(err, "Failed to sync HPA")
		}
	}

	return nil
}

func isMetricsServerInstalled(ctx context.Context, c client.Client) bool {
	// Check if Kubernetes metrics server is installed by checking if the metrics-server deployment exists
	logger := log.FromContext(ctx)
	var metricsServerDeployment appsv1.Deployment
	err := c.Get(ctx, types.NamespacedName{Name: "metrics-server", Namespace: "kube-system"}, &metricsServerDeployment)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			logger.Error(err, "Failed to get metrics-server deployment")
			return false
		}

		logger.V(0).Info("Metrics server not found, skipping HPA creation")
		return false
	}

	logger.V(0).Info("Metrics server found, creating HPA for Gateway")
	return true
}

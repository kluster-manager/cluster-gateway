package v1alpha1

import (
	"context"
	"fmt"
	"strconv"

	"github.com/kluster-manager/cluster-gateway/pkg/common"
	"github.com/kluster-manager/cluster-gateway/pkg/util/singleton"

	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/klog/v2"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	"sigs.k8s.io/apiserver-runtime/pkg/builder/resource"
	contextutil "sigs.k8s.io/apiserver-runtime/pkg/util/context"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ resource.ArbitrarySubResource = &ClusterGatewayHealth{}
var _ rest.Getter = &ClusterGatewayHealth{}
var _ rest.Updater = &ClusterGatewayHealth{}

type ClusterGatewayHealth ClusterGateway

func (in *ClusterGatewayHealth) New() runtime.Object {
	return &ClusterGateway{}
}

func (in *ClusterGatewayHealth) SubResourceName() string {
	return "health"
}

func (in *ClusterGatewayHealth) Destroy() {}

func (in *ClusterGatewayHealth) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	parentStorage, ok := contextutil.GetParentStorageGetter(ctx)
	if !ok {
		return nil, fmt.Errorf("no parent storage found")
	}
	parentObj, err := parentStorage.Get(ctx, name, options)
	if err != nil {
		return nil, fmt.Errorf("no such cluster %v", name)
	}
	clusterGateway := parentObj.(*ClusterGateway)
	return clusterGateway, nil
}

func (in *ClusterGatewayHealth) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	if singleton.GetClient() == nil {
		return nil, false, fmt.Errorf("controller manager is not initialized yet")
	}

	updating, err := objInfo.UpdatedObject(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	updatingClusterGateway := updating.(*ClusterGateway)

	var gwAddon addonv1alpha1.ManagedClusterAddOn
	err = singleton.GetClient().Get(ctx, types.NamespacedName{Name: common.AddonName, Namespace: name}, &gwAddon)
	if err != nil {
		return nil, false, err
	}
	mod := gwAddon.DeepCopy()
	if mod.Annotations == nil {
		mod.Annotations = make(map[string]string)
	}
	mod.Annotations[common.AnnotationKeyClusterGatewayStatusHealthy] = strconv.FormatBool(updatingClusterGateway.Status.Healthy)
	mod.Annotations[common.AnnotationKeyClusterGatewayStatusHealthyReason] = string(updatingClusterGateway.Status.HealthyReason)

	patch := client.MergeFrom(&gwAddon)
	err = singleton.GetClient().Patch(ctx, mod, patch)
	if err != nil {
		return nil, false, err
	}

	var cluster clusterv1.ManagedCluster
	err = singleton.GetClient().Get(ctx, types.NamespacedName{Name: name}, &cluster)
	if err != nil {
		return nil, false, err
	}

	endpointType := getClusterEndpointType(ctx, cluster.Name)

	var secret core.Secret
	err = singleton.GetClient().Get(ctx, types.NamespacedName{Name: common.AddonName, Namespace: cluster.Name}, &secret)
	if err != nil {
		klog.Warningf("Failed getting secret %q/%q: %v", cluster.Name, common.AddonName, err)
		return nil, false, err
	}

	clusterGateway, err := convert(&cluster, &gwAddon, endpointType, &secret)
	if err != nil {
		return nil, false, err
	}
	return clusterGateway, false, nil
}

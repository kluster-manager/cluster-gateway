package framework

import (
	configv1alpha1 "github.com/kluster-manager/cluster-gateway/pkg/apis/config/v1alpha1"
	gatewayv1alpha1 "github.com/kluster-manager/cluster-gateway/pkg/apis/gateway/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
)

var scheme = runtime.NewScheme()

func init() {
	gatewayv1alpha1.AddToScheme(scheme)
	configv1alpha1.AddToScheme(scheme)
	clusterv1.Install(scheme)
	addonv1alpha1.Install(scheme)
}

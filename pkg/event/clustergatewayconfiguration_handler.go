package event

import (
	"context"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	configv1alpha1 "github.com/kluster-manager/cluster-gateway/pkg/apis/config/v1alpha1"
)

var _ handler.EventHandler = &ClusterGatewayConfigurationHandler{}

type ClusterGatewayConfigurationHandler struct {
	client.Client
}

func (c *ClusterGatewayConfigurationHandler) Create(ctx context.Context, event event.CreateEvent, q workqueue.RateLimitingInterface) {
	cfg := event.Object.(*configv1alpha1.ClusterGatewayConfiguration)
	c.process(ctx, cfg, q)
}

func (c *ClusterGatewayConfigurationHandler) Update(ctx context.Context, event event.UpdateEvent, q workqueue.RateLimitingInterface) {
	cfg := event.ObjectNew.(*configv1alpha1.ClusterGatewayConfiguration)
	c.process(ctx, cfg, q)
}

func (c *ClusterGatewayConfigurationHandler) Delete(ctx context.Context, event event.DeleteEvent, q workqueue.RateLimitingInterface) {
	cfg := event.Object.(*configv1alpha1.ClusterGatewayConfiguration)
	c.process(ctx, cfg, q)
}

func (c *ClusterGatewayConfigurationHandler) Generic(ctx context.Context, event event.GenericEvent, q workqueue.RateLimitingInterface) {
	cfg := event.Object.(*configv1alpha1.ClusterGatewayConfiguration)
	c.process(ctx, cfg, q)
}

func (c *ClusterGatewayConfigurationHandler) process(ctx context.Context, config *configv1alpha1.ClusterGatewayConfiguration, q workqueue.RateLimitingInterface) {
	list := addonv1alpha1.ClusterManagementAddOnList{}

	if err := c.Client.List(ctx, &list); err != nil {
		ctrl.Log.WithName("ClusterGatewayConfiguration").Error(err, "failed list addons")
		return
	}

	for _, addon := range list.Items {
		for _, ref := range addon.Spec.SupportedConfigs {
			if ref.ConfigGroupResource.Group != configv1alpha1.GroupVersion.Group ||
				ref.ConfigGroupResource.Resource != "clustergatewayconfigurations" {
				continue
			}
			if ref.DefaultConfig != nil && ref.DefaultConfig.Name == config.Name {
				q.Add(reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name: addon.Name,
					},
				})
			}
		}
	}
}

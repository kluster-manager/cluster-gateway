package event

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/kluster-manager/cluster-gateway/pkg/common"
)

func AddOnHealthResyncHandler(c client.Client, interval time.Duration) source.Source {
	ch := StartBackgroundExternalTimerResync(func() ([]event.GenericEvent, error) {
		addonList := &addonv1alpha1.ManagedClusterAddOnList{}
		if err := c.List(context.TODO(), addonList); err != nil {
			return nil, err
		}
		evs := make([]event.GenericEvent, 0)
		for _, addon := range addonList.Items {
			if addon.Name != common.AddonName {
				continue
			}
			addon := addon
			evs = append(evs, event.GenericEvent{
				Object: &addon,
			})
		}
		return evs, nil
	}, interval)
	return source.Channel[client.Object](ch, GenericEventHandler{})
}

type GeneratorFunc func() ([]event.GenericEvent, error)

func StartBackgroundExternalTimerResync(g GeneratorFunc, interval time.Duration) <-chan event.GenericEvent {
	events := make(chan event.GenericEvent) // unbuffered
	ticker := time.NewTicker(interval)
	go func() {
		for {
			_, ok := <-ticker.C
			if !ok {
				return
			}
			evs, err := g()
			if err != nil {
				klog.Errorf("Encountered an error when getting periodic events: %v", err)
				continue
			}
			for _, ev := range evs {
				events <- ev
			}
		}
	}()
	return events
}

var _ handler.EventHandler = &GenericEventHandler{}

type GenericEventHandler struct {
}

func (a GenericEventHandler) Generic(_ context.Context, e event.TypedGenericEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	w.Add(reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: e.Object.GetNamespace(),
			Name:      e.Object.GetName(),
		},
	})
}

func (a GenericEventHandler) Create(_ context.Context, e event.TypedCreateEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	panic("implement me")
}

func (a GenericEventHandler) Update(_ context.Context, e event.TypedUpdateEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	panic("implement me")
}

func (a GenericEventHandler) Delete(_ context.Context, e event.TypedDeleteEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	panic("implement me")
}

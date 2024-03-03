package singleton

import (
	"github.com/kluster-manager/cluster-gateway/pkg/common"
	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/klog/v2/klogr"
	cu "kmodules.xyz/client-go/client"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var kc client.Client

func GetClient() client.Client {
	return kc
}

func SetClientForTesting(cc client.Client) {
	kc = cc
}

func InitManager(scheme *runtime.Scheme) server.PostStartHookFunc {
	return func(ctx server.PostStartHookContext) error {
		log.SetLogger(klogr.New()) // nolint:staticcheck

		cfg, err := ctrl.GetConfig()
		if err != nil {
			return errors.Wrap(err, "failed ctrl.GetConfig()")
		}

		mgr, err := manager.New(cfg, manager.Options{
			Scheme:                 scheme,
			Metrics:                metricsserver.Options{BindAddress: ""},
			HealthProbeBindAddress: "",
			LeaderElection:         false,
			LeaderElectionID:       "5b87adeb-hub.proxyserver.licenses.appscode.com",
			NewClient:              cu.NewClient,
			Cache: cache.Options{
				ByObject: map[client.Object]cache.ByObject{
					&core.Secret{}: {
						Field: fields.OneTermEqualSelector("metadata.name", common.AddonName),
					},
				},
			},
		})
		if err != nil {
			return errors.Wrap(err, "unable to start manager")
		}
		kc = mgr.GetClient()
		return mgr.Start(wait.ContextForChannel(ctx.StopCh))
	}
}

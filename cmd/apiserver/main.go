/*
Copyright 2021 The KubeVela Authors.

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

package main

import (
	"net/http"

	configv1alpha1 "github.com/kluster-manager/cluster-gateway/pkg/apis/config/v1alpha1"
	gatewayv1alpha1 "github.com/kluster-manager/cluster-gateway/pkg/apis/gateway/v1alpha1"
	"github.com/kluster-manager/cluster-gateway/pkg/common"
	"github.com/kluster-manager/cluster-gateway/pkg/config"
	_ "github.com/kluster-manager/cluster-gateway/pkg/featuregates"
	"github.com/kluster-manager/cluster-gateway/pkg/generated/openapi"
	"github.com/kluster-manager/cluster-gateway/pkg/metrics"
	"github.com/kluster-manager/cluster-gateway/pkg/util/singleton"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/server"
	genericfilters "k8s.io/apiserver/pkg/server/filters"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	cu "kmodules.xyz/client-go/client"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	ocmauthv1beta1 "open-cluster-management.io/managed-serviceaccount/apis/authentication/v1beta1"
	"sigs.k8s.io/apiserver-runtime/pkg/builder"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	clientgoscheme.AddToScheme(scheme)
	clusterv1.Install(scheme)
	addonv1alpha1.Install(scheme)
	ocmauthv1beta1.AddToScheme(scheme)
	gatewayv1alpha1.AddToScheme(scheme)
	configv1alpha1.AddToScheme(scheme)
}

func main() {

	// registering metrics
	metrics.Register()

	var mgr ctrl.Manager
	cmd, err := builder.APIServer.
		// +kubebuilder:scaffold:resource-register
		WithResource(&gatewayv1alpha1.ClusterGateway{}).
		WithLocalDebugExtension().
		ExposeLoopbackMasterClientConfig().
		ExposeLoopbackAuthorizer().
		WithoutEtcd().
		WithConfigFns(
			func(config *server.RecommendedConfig) *server.RecommendedConfig {
				config.LongRunningFunc = func(r *http.Request, requestInfo *request.RequestInfo) bool {
					if requestInfo.Resource == "clustergateways" && requestInfo.Subresource == "proxy" {
						return true
					}
					return genericfilters.BasicLongRunningRequestCheck(sets.NewString("watch"), sets.NewString())(r, requestInfo)
				}
				return config
			},
			config.WithUserAgent,
			func(config *server.RecommendedConfig) *server.RecommendedConfig {
				var err error
				mgr, err = manager.New(config.ClientConfig, manager.Options{
					Scheme:                 scheme,
					Metrics:                metricsserver.Options{BindAddress: ""},
					HealthProbeBindAddress: "",
					LeaderElection:         false,
					LeaderElectionID:       "cluster-gateway-apiserver",
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
					klog.Fatal("unable to create manager", err)
				}
				return config
			}).
		WithOpenAPIDefinitions(common.AddonName, "v0.0.1", openapi.GetOpenAPIDefinitions).
		WithOptionsFns(func(options *builder.ServerOptions) *builder.ServerOptions {
			if err := config.ValidateClusterProxy(); err != nil {
				klog.Fatal(err)
			}
			if err := gatewayv1alpha1.LoadGlobalClusterGatewayProxyConfig(); err != nil {
				klog.Fatal(err)
			}
			return options
		}).
		WithServerFns(func(server *builder.GenericAPIServer) *builder.GenericAPIServer {
			server.Handler.FullHandlerChain = gatewayv1alpha1.NewClusterGatewayProxyRequestEscaper(server.Handler.FullHandlerChain)
			return server
		}).
		WithPostStartHook("init-controller-manager", func(ctx server.PostStartHookContext) error {
			singleton.SetClient(mgr.GetClient())
			return mgr.Start(wait.ContextForChannel(ctx.StopCh))
		}).
		Build()
	if err != nil {
		klog.Fatal(err)
	}
	config.AddLogFlags(cmd.Flags())
	config.AddClusterProxyFlags(cmd.Flags())
	config.AddProxyAuthorizationFlags(cmd.Flags())
	config.AddUserAgentFlags(cmd.Flags())
	config.AddClusterGatewayProxyConfig(cmd.Flags())
	if err := cmd.Execute(); err != nil {
		klog.Fatal(err)
	}
}

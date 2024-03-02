package main

import (
	"context"
	"flag"
	"os"

	"github.com/oam-dev/cluster-gateway/pkg/addon/agent"
	"github.com/oam-dev/cluster-gateway/pkg/addon/controllers"
	proxyv1alpha1 "github.com/oam-dev/cluster-gateway/pkg/apis/proxy/v1alpha1"
	"github.com/oam-dev/cluster-gateway/pkg/util"
	"github.com/oam-dev/cluster-gateway/pkg/util/cert"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/klogr"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	"open-cluster-management.io/addon-framework/pkg/addonmanager"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	ocmauthv1beta1 "open-cluster-management.io/managed-serviceaccount/apis/authentication/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	addonv1alpha1.AddToScheme(scheme)
	proxyv1alpha1.AddToScheme(scheme)
	clientgoscheme.AddToScheme(scheme)
	apiregistrationv1.AddToScheme(scheme)
	ocmauthv1beta1.AddToScheme(scheme)
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var signerSecretName string
	var mcKubeconfig string

	logger := klogr.New()
	klog.SetOutput(os.Stdout)
	klog.InitFlags(flag.CommandLine)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":48080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":48081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&signerSecretName, "signer-secret-name", "cluster-gateway-signer",
		"The name of the secret to store the signer CA")
	flag.StringVar(&mcKubeconfig, "multicluster-kubeconfig", "",
		"The path to multicluster-controlplane kubeconfig")

	flag.Parse()
	ctrl.SetLogger(logger)

	var mcConfig, hostConfig *rest.Config

	if mcKubeconfig != "" {
		var err error
		mcConfig, err = clientcmd.BuildConfigFromFlags("", mcKubeconfig)
		if err != nil {
			setupLog.Error(err, "unable to build multicluster rest config")
			os.Exit(1)
		}
		hostConfig = ctrl.GetConfigOrDie()
	} else {
		hostConfig = ctrl.GetConfigOrDie()
		mcConfig = hostConfig
	}

	mgr, err := ctrl.NewManager(mcConfig, ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "cluster-gateway-addon-manager",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	currentNamespace := os.Getenv("NAMESPACE")
	if len(currentNamespace) == 0 {
		inClusterNamespace, err := util.GetInClusterNamespace()
		if err != nil {
			klog.Fatal("the manager should be either running in a container or specify NAMESPACE environment")
		}
		currentNamespace = inClusterNamespace
	}

	caPair, err := cert.EnsureCAPair(hostConfig, currentNamespace, signerSecretName)
	if err != nil {
		setupLog.Error(err, "unable to ensure ca signer")
	}
	hostClient, err := kubernetes.NewForConfig(hostConfig)
	if err != nil {
		setupLog.Error(err, "unable to set up host kubernetes native client")
		os.Exit(1)
	}

	hostKubeClient, err := newHostClient(hostConfig)
	if err != nil {
		setupLog.Error(err, "failed create host KubeClient")
		os.Exit(1)
	}

	hostInformerFactory := informers.NewSharedInformerFactory(hostClient, 0)
	if err := controllers.SetupClusterGatewayInstallerWithManager(
		mgr,
		caPair,
		hostKubeClient,
		hostClient,
		hostInformerFactory.Core().V1().Secrets().Lister(),
		mcKubeconfig != ""); err != nil {
		setupLog.Error(err, "unable to setup installer")
		os.Exit(1)
	}
	if err := controllers.SetupClusterGatewayHealthProberWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to setup health prober")
		os.Exit(1)
	}

	ctx := context.Background()
	go hostInformerFactory.Start(ctx.Done())

	addonManager, err := addonmanager.New(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
	if err := addonManager.AddAgent(agent.NewClusterGatewayAddonManager(
		mgr.GetConfig(),
		mgr.GetClient(),
	)); err != nil {
		setupLog.Error(err, "unable to register addon manager")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(ctrl.SetupSignalHandler())
	defer cancel()
	go addonManager.Start(ctx)

	if err := mgr.Start(ctx); err != nil {
		panic(err)
	}

}

func newHostClient(hostConfig *rest.Config) (client.Client, error) {
	hc, err := rest.HTTPClientFor(hostConfig)
	if err != nil {
		return nil, err
	}
	mapper, err := apiutil.NewDynamicRESTMapper(hostConfig, hc)
	if err != nil {
		return nil, err
	}
	return client.New(hostConfig, client.Options{
		Scheme: clientgoscheme.Scheme,
		Mapper: mapper,
	})
}

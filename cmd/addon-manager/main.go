package main

import (
	"context"
	"flag"
	"os"

	"github.com/kluster-manager/cluster-gateway/pkg/addon/agent"
	"github.com/kluster-manager/cluster-gateway/pkg/addon/controllers"
	configv1alpha1 "github.com/kluster-manager/cluster-gateway/pkg/apis/config/v1alpha1"
	"github.com/kluster-manager/cluster-gateway/pkg/util"
	"github.com/kluster-manager/cluster-gateway/pkg/util/cert"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	nativescheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/klogr"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	"open-cluster-management.io/addon-framework/pkg/addonmanager"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	ocmauthv1beta1 "open-cluster-management.io/managed-serviceaccount/apis/authentication/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	addonv1alpha1.Install(scheme)
	configv1alpha1.AddToScheme(scheme)
	nativescheme.AddToScheme(scheme)
	apiregistrationv1.AddToScheme(scheme)
	ocmauthv1beta1.AddToScheme(scheme)
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var signerSecretName string
	var mcKubeconfig string
	var mcKubeconfigSecretName string

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
	flag.StringVar(&mcKubeconfigSecretName, "multicluster-kubeconfig-secret-name", "",
		"The name of multicluster-controlplane kubeconfig secret")

	flag.Parse()
	ctrl.SetLogger(logger)

	hostManager, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "cluster-gateway-manager",
	})
	if err != nil {
		setupLog.Error(err, "unable to create host manager")
		os.Exit(1)
	}

	podNamespace := os.Getenv("POD_NAMESPACE")
	if len(podNamespace) == 0 {
		inClusterNamespace, err := util.GetInClusterNamespace()
		if err != nil {
			klog.Fatal("the manager should be either running in a container or specify POD_NAMESPACE environment")
		}
		podNamespace = inClusterNamespace
	}
	var addonManagerNamespace string

	mcMode := mcKubeconfig != ""
	var mcManager ctrl.Manager
	if mcMode {
		mcConfig, err := clientcmd.BuildConfigFromFlags("", mcKubeconfig)
		if err != nil {
			setupLog.Error(err, "unable to build multicluster rest config")
			os.Exit(1)
		}
		mcManager, err = ctrl.NewManager(mcConfig, ctrl.Options{
			Scheme:                 scheme,
			Metrics:                metricsserver.Options{BindAddress: ""},
			HealthProbeBindAddress: "",
			LeaderElection:         enableLeaderElection,
			LeaderElectionID:       "cluster-gateway-manager",
		})
		if err != nil {
			setupLog.Error(err, "unable to create mc manager")
			os.Exit(1)
		}

		addonManagerNamespace = os.Getenv("NAMESPACE")
		if len(podNamespace) == 0 {
			klog.Fatal("addon manager namespace can't determined as NAMESPACE env variable is empty")
		}
		if len(mcKubeconfigSecretName) == 0 {
			klog.Fatal("missing flag --multicluster-kubeconfig-secret-name")
		}
	} else {
		mcManager = hostManager
		addonManagerNamespace = podNamespace
	}

	caPair, err := cert.EnsureCAPair(hostManager.GetConfig(), podNamespace, signerSecretName)
	if err != nil {
		setupLog.Error(err, "unable to ensure ca signer")
	}
	hostNativeClient, err := kubernetes.NewForConfig(hostManager.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to instantiate legacy client")
		os.Exit(1)
	}
	informerFactory := informers.NewSharedInformerFactory(hostNativeClient, 0)
	if err := controllers.SetupClusterGatewayInstallerWithManager(
		hostManager,
		podNamespace,
		caPair,
		hostNativeClient,
		informerFactory.Core().V1().Secrets().Lister(),
		mcManager,
		mcMode,
		mcKubeconfigSecretName,
		addonManagerNamespace); err != nil {
		setupLog.Error(err, "unable to setup installer")
		os.Exit(1)
	}
	if err := controllers.SetupClusterGatewayHealthProberWithManager(mcManager); err != nil {
		setupLog.Error(err, "unable to setup health prober")
		os.Exit(1)
	}

	ctx := context.Background()
	go informerFactory.Start(ctx.Done())

	addonManager, err := addonmanager.New(mcManager.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
	if err := addonManager.AddAgent(agent.NewClusterGatewayAddonManager(
		mcManager.GetConfig(),
		mcManager.GetClient(),
	)); err != nil {
		setupLog.Error(err, "unable to register addon manager")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(ctrl.SetupSignalHandler())
	defer cancel()
	go addonManager.Start(ctx)
	if mcMode {
		go mcManager.Start(ctx)
	}
	if err := hostManager.Start(ctx); err != nil {
		panic(err)
	}
}

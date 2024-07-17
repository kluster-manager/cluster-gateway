package controllers

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/openshift/library-go/pkg/crypto"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	corev1lister "k8s.io/client-go/listers/core/v1"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	"k8s.io/utils/ptr"
	"open-cluster-management.io/addon-framework/pkg/certrotation"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	ocmauthv1beta1 "open-cluster-management.io/managed-serviceaccount/apis/authentication/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	configv1alpha1 "github.com/kluster-manager/cluster-gateway/pkg/apis/config/v1alpha1"
	"github.com/kluster-manager/cluster-gateway/pkg/common"
	eu "github.com/kluster-manager/cluster-gateway/pkg/event"
	"github.com/kluster-manager/cluster-gateway/pkg/util/cert"
)

var (
	log = ctrl.Log.WithName("ClusterGatewayInstaller")
)
var _ reconcile.Reconciler = &ClusterGatewayInstaller{}

func SetupClusterGatewayInstallerWithManager(
	hostManager ctrl.Manager,
	podNamespace string,
	caPair *crypto.CA,
	hostNativeClient kubernetes.Interface,
	hostSecretLister corev1lister.SecretLister,
	mcManager ctrl.Manager,
	mcMode bool,
	mcKubeconfigSecretName string,
	addonManagerNamespace string,
) error {
	installer := &ClusterGatewayInstaller{
		hostNativeClient:       hostNativeClient,
		podNamespace:           podNamespace,
		caPair:                 caPair,
		hostSecretLister:       hostSecretLister,
		hostRtc:                hostManager.GetClient(),
		mcRtc:                  mcManager.GetClient(),
		mcMode:                 mcMode,
		mcKubeconfigSecretName: mcKubeconfigSecretName,
		addonManagerNamespace:  addonManagerNamespace,
	}
	apiServiceHandler := func(ctx context.Context, object client.Object) []reconcile.Request {
		apiService := object.(*apiregistrationv1.APIService)
		var reqs []reconcile.Request
		if apiService.Name == common.ClusterGatewayAPIServiceName {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: common.AddonName,
				},
			})
		}
		return reqs
	}
	gatewayConfigHandler := func(ctx context.Context, object client.Object) []reconcile.Request {
		config := object.(*configv1alpha1.ClusterGatewayConfiguration)
		var reqs []reconcile.Request

		list := addonv1alpha1.ClusterManagementAddOnList{}
		if err := mcManager.GetClient().List(ctx, &list); err != nil {
			ctrl.Log.WithName("ClusterGatewayConfiguration").Error(err, "failed list addons")
			return reqs
		}
		for _, addon := range list.Items {
			for _, ref := range addon.Spec.SupportedConfigs {
				if ref.ConfigGroupResource.Group != configv1alpha1.GroupVersion.Group ||
					ref.ConfigGroupResource.Resource != "clustergatewayconfigurations" {
					continue
				}
				if ref.DefaultConfig != nil && ref.DefaultConfig.Name == config.Name {
					reqs = append(reqs, reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name: addon.Name,
						},
					})
				}
			}
		}
		return reqs
	}
	secretHandler := func(ctx context.Context, object client.Object) []reconcile.Request {
		secret := object.(*corev1.Secret)
		var reqs []reconcile.Request
		for _, ref := range secret.OwnerReferences {
			if ref.Kind == "ManagedServiceAccount" && ref.Name == common.AddonName {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name: common.AddonName,
					},
				})
			}
		}
		return reqs
	}

	builder := ctrl.NewControllerManagedBy(mcManager).
		// Watches ClusterManagementAddOn singleton
		For(&addonv1alpha1.ClusterManagementAddOn{}).
		// Watches ClusterGatewayConfiguration singleton
		Watches(&configv1alpha1.ClusterGatewayConfiguration{}, handler.EnqueueRequestsFromMapFunc(gatewayConfigHandler)).
		// Watches ManagedClusterAddon.
		Owns(&addonv1alpha1.ManagedClusterAddOn{}).
		// Secrets rotated by ManagedServiceAccount should be actively reconciled
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(secretHandler)).
		// APIService should be actively reconciled
		Watches(&apiregistrationv1.APIService{}, handler.EnqueueRequestsFromMapFunc(apiServiceHandler))

	if mcMode {
		src := setupHostWatcher(hostManager)
		builder = builder.
			WatchesRawSource(src)
	} else {
		builder = builder.
			// Cluster-Gateway mTLS certificate should be actively reconciled
			Owns(&corev1.Secret{}).
			// Cluster-gateway apiserver instances should be actively reconciled
			Owns(&appsv1.Deployment{})
	}
	return builder.Complete(installer)
}

type HostReconciler struct {
	events chan event.GenericEvent
}

var _ reconcile.Reconciler = &HostReconciler{}

func (r *HostReconciler) Reconcile(_ context.Context, req reconcile.Request) (reconcile.Result, error) {
	var u unstructured.Unstructured
	u.SetNamespace(req.Namespace)
	u.SetName(req.Name)
	r.events <- event.GenericEvent{
		Object: &u,
	}
	return reconcile.Result{}, nil
}

func setupHostWatcher(hostManager ctrl.Manager) source.Source {
	events := make(chan event.GenericEvent) // unbuffered
	r := &HostReconciler{
		events: events,
	}
	ctrl.NewControllerManagedBy(hostManager).
		// Cluster-Gateway mTLS certificate should be actively reconciled
		Owns(&corev1.Secret{}).
		// Cluster-gateway apiserver instances should be actively reconciled
		Owns(&appsv1.Deployment{}).
		Complete(r)
	return source.Channel[client.Object](events, eu.GenericEventHandler{})
}

type ClusterGatewayInstaller struct {
	hostNativeClient kubernetes.Interface
	hostSecretLister corev1lister.SecretLister
	caPair           *crypto.CA
	hostRtc          client.Client
	podNamespace     string

	mcRtc                  client.Client
	mcMode                 bool
	mcKubeconfigSecretName string
	addonManagerNamespace  string
}

const (
	SecretNameClusterGatewayTLSCert = "cluster-gateway-tls-cert"
	ServiceNameClusterGateway       = "cluster-gateway"
)

func (c *ClusterGatewayInstaller) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	// get the cluster-management-addon instance
	log.Info("Start reconciling")
	addon := &addonv1alpha1.ClusterManagementAddOn{}
	if err := c.mcRtc.Get(ctx, request.NamespacedName, addon); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, errors.Wrapf(err, "failed to get cluster-management-addon: %v", request.Name)
	}
	if addon.Name != common.AddonName {
		// skip
		return reconcile.Result{}, nil
	}

	var foundConfig bool
	var clusterGatewayConfiguration configv1alpha1.ClusterGatewayConfiguration
	for _, ref := range addon.Spec.SupportedConfigs {
		if ref.ConfigGroupResource.Group != configv1alpha1.GroupVersion.Group ||
			ref.ConfigGroupResource.Resource != "clustergatewayconfigurations" {
			continue
		}

		foundConfig = true
		if ref.DefaultConfig != nil {
			if err := c.mcRtc.Get(context.TODO(), types.NamespacedName{Name: ref.DefaultConfig.Name}, &clusterGatewayConfiguration); err != nil {
				if apierrors.IsNotFound(err) {
					return reconcile.Result{}, fmt.Errorf("no such configuration: %v", ref.DefaultConfig.Name)
				}
				return reconcile.Result{}, fmt.Errorf("failed getting configuration: %v", ref.DefaultConfig.Name)
			}
			break
		}
	}
	if !foundConfig {
		// skip
		return reconcile.Result{}, nil
	}

	if err := c.ensureNamespace(c.podNamespace); err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to ensure required namespace")
	}
	if err := c.ensureClusterProxySecrets(&clusterGatewayConfiguration); err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to ensure required proxy client related credentials")
	}
	if err := c.ensureSecretManagement(addon, &clusterGatewayConfiguration); err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to configure secret management")
	}

	sans := []string{
		ServiceNameClusterGateway,
		ServiceNameClusterGateway + "." + c.podNamespace,
		ServiceNameClusterGateway + "." + c.podNamespace + ".svc",
	}
	if c.podNamespace != c.addonManagerNamespace {
		sans = append(sans,
			ServiceNameClusterGateway+"."+c.addonManagerNamespace,
			ServiceNameClusterGateway+"."+c.addonManagerNamespace+".svc",
		)
	}
	rotation := certrotation.TargetRotation{
		Namespace: c.podNamespace,
		Name:      SecretNameClusterGatewayTLSCert,
		HostNames: sans,
		Validity:  time.Hour * 24 * 180,
		Lister:    c.hostSecretLister,
		Client:    c.hostNativeClient.CoreV1(),
	}
	if err := rotation.EnsureTargetCertKeyPair(c.caPair, c.caPair.Config.Certs); err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed rotating server tls cert")
	}

	owner := addon
	if c.mcMode {
		owner = &addonv1alpha1.ClusterManagementAddOn{}
		if err := c.hostRtc.Get(ctx, request.NamespacedName, owner); err != nil {
			if apierrors.IsNotFound(err) {
				return reconcile.Result{}, nil
			}
			return reconcile.Result{}, errors.Wrapf(err, "failed to get host cluster-management-addon: %v", request.Name)
		}
	}

	// create if not exists
	namespace := c.podNamespace
	targets := []client.Object{
		newServiceAccount(owner, namespace),
		newClusterGatewayService(owner, namespace),
		newAuthenticationRole(owner, namespace),
		newAuthDelegatorRole(owner, namespace),
		newAPFClusterRole(owner),
		newAPFClusterRoleBinding(owner, namespace),
	}
	for _, obj := range targets {
		if err := c.hostRtc.Create(context.TODO(), obj); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return reconcile.Result{}, errors.Wrapf(err, "failed deploying host cluster-gateway components")
			}
		}
	}

	if err := c.ensureClusterGatewayDeployment(owner, &clusterGatewayConfiguration); err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed ensuring cluster-gateway deployment")
	}

	// /Users/tamal/go/src/kubeops.dev/installer/charts/kube-ui-server/templates/ocm-mc/svc.yaml
	if c.mcMode {
		var hostSvc corev1.Service
		if err := c.hostRtc.Get(context.TODO(), types.NamespacedName{
			Namespace: namespace,
			Name:      common.AddonName,
		}, &hostSvc); err != nil {
			if apierrors.IsNotFound(err) || hostSvc.Spec.ClusterIP == "" {
				return reconcile.Result{
					Requeue:      true,
					RequeueAfter: 5 * time.Second,
				}, nil
			}
		}

		mcTargets := []client.Object{
			newMCNamespace(c.addonManagerNamespace),
			newMCClusterGatewayService(addon, c.addonManagerNamespace, hostSvc.Spec.ClusterIP),
			newMCClusterGatewayEndpoints(addon, c.addonManagerNamespace, hostSvc.Spec.ClusterIP),
		}
		for _, obj := range mcTargets {
			if err := c.mcRtc.Create(context.TODO(), obj); err != nil {
				if !apierrors.IsAlreadyExists(err) && !(obj.GetObjectKind().GroupVersionKind().Kind == "Service" && apierrors.IsInvalid(err)) {
					return reconcile.Result{}, errors.Wrapf(err, "failed deploying mc cluster-gateway components")
				}
			}
		}
	}

	// always update apiservice
	if err := c.ensureAPIService(addon, c.addonManagerNamespace); err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed ensuring cluster-gateway apiservice")
	}

	return reconcile.Result{}, nil
}

func (c *ClusterGatewayInstaller) ensureNamespace(namespace string) error {
	ns := &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Namespace",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}
	if _, err := c.hostNativeClient.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func (c *ClusterGatewayInstaller) ensureAPIService(addon *addonv1alpha1.ClusterManagementAddOn, namespace string) error {
	caCertData, _, err := c.caPair.Config.GetPEMBytes()
	if err != nil {
		return err
	}
	expected := newAPIService(addon, namespace, caCertData)
	current := &apiregistrationv1.APIService{}
	err = c.mcRtc.Get(context.TODO(), types.NamespacedName{
		Name: expected.Name,
	}, current)
	if apierrors.IsNotFound(err) {
		if err := c.mcRtc.Create(context.TODO(), expected); err != nil {
			return errors.Wrapf(err, "failed to create cluster-gateway apiservice")
		}
	} else if err != nil {
		return err
	}
	if !bytes.Equal(caCertData, current.Spec.CABundle) {
		expected.ResourceVersion = current.ResourceVersion
		if err := c.mcRtc.Update(context.TODO(), expected); err != nil {
			return err
		}
	}
	return nil
}

func (c *ClusterGatewayInstaller) ensureClusterGatewayDeployment(addon *addonv1alpha1.ClusterManagementAddOn, config *configv1alpha1.ClusterGatewayConfiguration) error {
	currentClusterGateway := &appsv1.Deployment{}
	if err := c.hostRtc.Get(context.TODO(), types.NamespacedName{
		Namespace: c.podNamespace,
		Name:      common.AddonName,
	}, currentClusterGateway); err != nil {
		if apierrors.IsNotFound(err) {
			clusterGateway := newClusterGatewayDeployment(addon, config, c.podNamespace, c.mcKubeconfigSecretName)
			if err := c.hostRtc.Create(context.TODO(), clusterGateway); err != nil {
				return err
			}
			return nil
		}
		return err
	}
	genStr, ok := currentClusterGateway.Labels[labelKeyClusterGatewayConfigurationGeneration]
	if ok {
		gen, err := strconv.Atoi(genStr)
		if err != nil {
			return err
		}
		if config.Generation == int64(gen) {
			return nil
		}
	}

	clusterGateway := newClusterGatewayDeployment(addon, config, c.podNamespace, c.mcKubeconfigSecretName)
	clusterGateway.ResourceVersion = currentClusterGateway.ResourceVersion
	if err := c.hostRtc.Update(context.TODO(), clusterGateway); err != nil {
		return err
	}
	return nil
}

func (c *ClusterGatewayInstaller) ensureClusterProxySecrets(config *configv1alpha1.ClusterGatewayConfiguration) error {
	if config.Spec.Egress.ClusterProxy == nil {
		return nil
	}
	if config.Spec.Egress.ClusterProxy.Credentials.Namespace == c.podNamespace {
		return nil
	}
	proxyClientCASecretName := config.Spec.Egress.ClusterProxy.Credentials.ProxyClientCASecretName
	err := cert.CopySecret(c.hostNativeClient,
		config.Spec.Egress.ClusterProxy.Credentials.Namespace, proxyClientCASecretName,
		c.podNamespace, proxyClientCASecretName)
	if err != nil {
		return errors.Wrapf(err, "failed copy secret %v", proxyClientCASecretName)
	}
	proxyClientSecretName := config.Spec.Egress.ClusterProxy.Credentials.ProxyClientSecretName
	err = cert.CopySecret(c.hostNativeClient,
		config.Spec.Egress.ClusterProxy.Credentials.Namespace, proxyClientSecretName,
		c.podNamespace, proxyClientSecretName)
	if err != nil {
		return errors.Wrapf(err, "failed copy secret %v", proxyClientSecretName)
	}
	return nil
}

func (c *ClusterGatewayInstaller) ensureSecretManagement(clusterAddon *addonv1alpha1.ClusterManagementAddOn, config *configv1alpha1.ClusterGatewayConfiguration) error {
	if config.Spec.SecretManagement.Type != configv1alpha1.SecretManagementTypeManagedServiceAccount {
		return nil
	}
	if _, err := c.mcRtc.RESTMapper().KindFor(schema.GroupVersionResource{
		Group:    ocmauthv1beta1.GroupVersion.Group,
		Version:  ocmauthv1beta1.GroupVersion.Version,
		Resource: "managedserviceaccounts",
	}); err != nil {
		return fmt.Errorf("failed to discover ManagedServiceAccount resource in the cluster")
	}
	addonList := &addonv1alpha1.ManagedClusterAddOnList{}
	if err := c.mcRtc.List(context.TODO(), addonList); err != nil {
		return errors.Wrapf(err, "failed to list managed cluster addons")
	}
	clusterGatewayAddon := make([]*addonv1alpha1.ManagedClusterAddOn, 0)
	for _, addon := range addonList.Items {
		addon := addon
		if addon.Name == common.AddonName {
			clusterGatewayAddon = append(clusterGatewayAddon, &addon)
		}
	}
	for _, addon := range clusterGatewayAddon {
		managedServiceAccount := buildManagedServiceAccount(addon)
		if err := c.mcRtc.Create(context.TODO(), managedServiceAccount); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return errors.Wrapf(err, "failed to create managed serviceaccount")
			}
		}
	}
	return nil
}

func newMCNamespace(namespace string) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Namespace",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}
}

func newServiceAccount(owner *addonv1alpha1.ClusterManagementAddOn, namespace string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ServiceAccount",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      common.AddonName,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: addonv1alpha1.GroupVersion.String(),
					Kind:       "ClusterManagementAddOn",
					UID:        owner.UID,
					Name:       owner.Name,
				},
			},
		},
	}
}

const labelKeyClusterGatewayConfigurationGeneration = "config.gateway.open-cluster-management.io/configuration-generation"

func newClusterGatewayDeployment(owner *addonv1alpha1.ClusterManagementAddOn, config *configv1alpha1.ClusterGatewayConfiguration, installNamespace, mcKubeconfigSecretName string) *appsv1.Deployment {
	args := []string{
		"--secure-port=9443",
		"--tls-cert-file=/etc/server/tls.crt",
		"--tls-private-key-file=/etc/server/tls.key",
		"--feature-gates=HealthinessCheck=true,ClientIdentityPenetration=true,ValidatingAdmissionPolicy=false",
	}
	volumes := []corev1.Volume{
		{
			Name: "server",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: SecretNameClusterGatewayTLSCert,
				},
			},
		},
	}
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "server",
			MountPath: "/etc/server/",
			ReadOnly:  true,
		},
	}
	if config.Spec.Egress.ClusterProxy != nil {
		args = append(args,
			"--proxy-host="+config.Spec.Egress.ClusterProxy.ProxyServerHost,
			"--proxy-port="+strconv.Itoa(int(config.Spec.Egress.ClusterProxy.ProxyServerPort)),
			"--proxy-ca-cert=/etc/ca/ca.crt",
			"--proxy-cert=/etc/tls/tls.crt",
			"--proxy-key=/etc/tls/tls.key",
		)
		volumes = append(volumes,
			corev1.Volume{
				Name: "proxy-client-ca",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: "proxy-server-ca",
					},
				},
			},
			corev1.Volume{
				Name: "proxy-client",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: "proxy-client",
					},
				},
			},
		)
		volumeMounts = append(volumeMounts,
			corev1.VolumeMount{
				Name:      "proxy-client-ca",
				MountPath: "/etc/ca/",
			},
			corev1.VolumeMount{
				Name:      "proxy-client",
				MountPath: "/etc/tls/",
			},
		)
	}

	// /Users/tamal/go/src/kubeops.dev/installer/charts/kube-ui-server/templates/deployment.yaml
	if mcKubeconfigSecretName != "" {
		args = append(args,
			"--kubeconfig=/var/run/secrets/ocm/auth/kubeconfig",
			"--authorization-kubeconfig=/var/run/secrets/ocm/auth/kubeconfig",
			"--authentication-kubeconfig=/var/run/secrets/ocm/auth/kubeconfig",
		)
		volumes = append(volumes,
			corev1.Volume{
				Name: "ocm-auth",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: mcKubeconfigSecretName,
					},
				},
			},
		)
		volumeMounts = append(volumeMounts,
			corev1.VolumeMount{
				Name:      "ocm-auth",
				MountPath: "/var/run/secrets/ocm/auth",
			},
		)
	}

	maxUnavailable := intstr.FromInt32(1)
	maxSurge := intstr.FromInt32(1)
	deploy := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: installNamespace,
			Name:      common.AddonName,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: addonv1alpha1.GroupVersion.String(),
					Kind:       "ClusterManagementAddOn",
					UID:        owner.UID,
					Name:       owner.Name,
				},
			},
			Labels: map[string]string{
				labelKeyClusterGatewayConfigurationGeneration: strconv.Itoa(int(config.Generation)),
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					common.LabelKeyOpenClusterManagementAddon: common.AddonName,
				},
			},
			Replicas: ptr.To(config.Spec.Replicas),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						common.LabelKeyOpenClusterManagementAddon: common.AddonName,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "apiserver",
							Image:           config.Spec.Image,
							ImagePullPolicy: corev1.PullAlways,
							Args:            args,
							VolumeMounts:    volumeMounts,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
									corev1.ResourceMemory: *resource.NewQuantity(200*1024*1024, resource.BinarySI),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    *resource.NewMilliQuantity(500, resource.DecimalSI),
									corev1.ResourceMemory: *resource.NewQuantity(600*1024*1024, resource.BinarySI),
								},
							},
						},
					},
					Volumes: volumes,
				},
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{

					MaxUnavailable: &maxUnavailable,
					MaxSurge:       &maxSurge,
				},
			},
		},
	}
	if mcKubeconfigSecretName == "" {
		deploy.Spec.Template.Spec.ServiceAccountName = common.AddonName
		deploy.Spec.Template.Spec.AutomountServiceAccountToken = ptr.To(true)
	} else {
		deploy.Spec.Template.Spec.AutomountServiceAccountToken = ptr.To(false)
	}
	return deploy
}

func newClusterGatewayService(owner *addonv1alpha1.ClusterManagementAddOn, namespace string) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      common.AddonName,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: addonv1alpha1.GroupVersion.String(),
					Kind:       "ClusterManagementAddOn",
					UID:        owner.UID,
					Name:       owner.Name,
				},
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				common.LabelKeyOpenClusterManagementAddon: common.AddonName,
			},
			Ports: []corev1.ServicePort{
				{
					Name: "https",
					Port: 9443,
				},
			},
		},
	}
}

func newMCClusterGatewayService(owner *addonv1alpha1.ClusterManagementAddOn, namespace string, hostSvcIP string) *corev1.Service {
	svc := newClusterGatewayService(owner, namespace)
	svc.Spec.ClusterIP = hostSvcIP
	return svc
}

func newMCClusterGatewayEndpoints(owner *addonv1alpha1.ClusterManagementAddOn, namespace string, hostSvcIP string) *corev1.Endpoints {
	return &corev1.Endpoints{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Endpoints",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      common.AddonName,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: addonv1alpha1.GroupVersion.String(),
					Kind:       "ClusterManagementAddOn",
					UID:        owner.UID,
					Name:       owner.Name,
				},
			},
		},
		Subsets: []corev1.EndpointSubset{
			{
				Addresses: []corev1.EndpointAddress{
					{
						IP: hostSvcIP,
					},
				},
				Ports: []corev1.EndpointPort{
					{
						Name:     "https",
						Port:     9443,
						Protocol: corev1.ProtocolTCP,
					},
				},
			},
		},
	}
}

func newAPIService(owner *addonv1alpha1.ClusterManagementAddOn, namespace string, verifyingCABundle []byte) *apiregistrationv1.APIService {
	return &apiregistrationv1.APIService{
		ObjectMeta: metav1.ObjectMeta{
			Name: "v1alpha1.gateway.open-cluster-management.io",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: addonv1alpha1.GroupVersion.String(),
					Kind:       "ClusterManagementAddOn",
					UID:        owner.UID,
					Name:       owner.Name,
				},
			},
		},
		Spec: apiregistrationv1.APIServiceSpec{
			Group:   "gateway.open-cluster-management.io",
			Version: "v1alpha1",
			Service: &apiregistrationv1.ServiceReference{
				Namespace: namespace,
				Name:      common.AddonName,
				Port:      ptr.To(int32(9443)),
			},
			GroupPriorityMinimum: 5000,
			VersionPriority:      10,
			CABundle:             verifyingCABundle,
		},
	}
}

func newAuthenticationRole(owner *addonv1alpha1.ClusterManagementAddOn, namespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "kube-system",
			Name:      "extension-apiserver-authentication-reader:cluster-gateway",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: addonv1alpha1.GroupVersion.String(),
					Kind:       "ClusterManagementAddOn",
					UID:        owner.UID,
					Name:       owner.Name,
				},
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind: "Role",
			Name: "extension-apiserver-authentication-reader",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Namespace: namespace,
				Name:      common.AddonName,
			},
		},
	}
}

func newAuthDelegatorRole(owner *addonv1alpha1.ClusterManagementAddOn, namespace string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "auth-delegator:cluster-gateway",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: addonv1alpha1.GroupVersion.String(),
					Kind:       "ClusterManagementAddOn",
					UID:        owner.UID,
					Name:       owner.Name,
				},
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind: "ClusterRole",
			Name: "system:auth-delegator",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Namespace: namespace,
				Name:      common.AddonName,
			},
		},
	}
}

func newAPFClusterRole(owner *addonv1alpha1.ClusterManagementAddOn) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: "open-cluster-management:cluster-gateway:apiserver",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: addonv1alpha1.GroupVersion.String(),
					Kind:       "ClusterManagementAddOn",
					UID:        owner.UID,
					Name:       owner.Name,
				},
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"cluster.open-cluster-management.io"},
				Resources: []string{"managedclusters"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"namespaces"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"admissionregistration.k8s.io"},
				Resources: []string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations", "validatingadmissionpolicies", "validatingadmissionpolicybindings"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"flowcontrol.apiserver.k8s.io"},
				Resources: []string{"prioritylevelconfigurations", "flowschemas"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"authorization.k8s.io"},
				Resources: []string{"subjectaccessreviews"},
				Verbs:     []string{"*"},
			},
			// read/update managed cluster addons
			{
				APIGroups: []string{"addon.open-cluster-management.io"},
				Resources: []string{"managedclusteraddons"},
				Verbs:     []string{"get", "list", "watch", "update", "patch"},
			},
			// read managed service account credentials
			{
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				Verbs:         []string{"get", "list", "watch"},
				ResourceNames: []string{common.AddonName},
			},
		},
	}
}

func newAPFClusterRoleBinding(owner *addonv1alpha1.ClusterManagementAddOn, namespace string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "open-cluster-management:cluster-gateway:apiserver",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: addonv1alpha1.GroupVersion.String(),
					Kind:       "ClusterManagementAddOn",
					UID:        owner.UID,
					Name:       owner.Name,
				},
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind: "ClusterRole",
			Name: "open-cluster-management:cluster-gateway:apiserver",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Namespace: namespace,
				Name:      common.AddonName,
			},
		},
	}
}

func buildManagedServiceAccount(owner *addonv1alpha1.ManagedClusterAddOn) *ocmauthv1beta1.ManagedServiceAccount {
	return &ocmauthv1beta1.ManagedServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "authentication.open-cluster-management.io/v1alpha1",
			Kind:       "ManagedServiceAccount",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: owner.Namespace,
			Name:      common.AddonName,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: addonv1alpha1.GroupVersion.String(),
					Kind:       "ManagedClusterAddOn",
					UID:        owner.UID,
					Name:       owner.Name,
				},
			},
		},
		Spec: ocmauthv1beta1.ManagedServiceAccountSpec{
			Rotation: ocmauthv1beta1.ManagedServiceAccountRotation{
				Enabled: true,
				Validity: metav1.Duration{
					Duration: time.Hour * 24 * 180,
				},
			},
		},
	}
}

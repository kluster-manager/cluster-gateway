package v1alpha1

import (
	"context"
	"fmt"
	"strconv"

	"github.com/kluster-manager/cluster-gateway/pkg/common"
	"github.com/kluster-manager/cluster-gateway/pkg/config"
	"github.com/kluster-manager/cluster-gateway/pkg/featuregates"
	"github.com/kluster-manager/cluster-gateway/pkg/util/singleton"

	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/registry/rest"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
)

var _ rest.Getter = &ClusterGateway{}
var _ rest.Lister = &ClusterGateway{}

// Conversion between corev1.Secret and ClusterGateway:
//  1. Storing credentials under the secret's data including X.509 key-pair or token.
//  2. Extending the spec of ClusterGateway by the secret's label.
//  3. Extending the status of ClusterGateway by the secrets' annotation.
//
// NOTE: Because the secret resource is designed to have no "metadata.generation" field,
// the ClusterGateway resource also misses the generation tracking.

func (in *ClusterGateway) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	if singleton.GetClient() == nil {
		return nil, fmt.Errorf("controller manager is not initialized yet")
	}

	var cluster clusterv1.ManagedCluster
	err := singleton.GetClient().Get(ctx, types.NamespacedName{Name: name}, &cluster)
	if err != nil {
		return nil, err
	}

	var gwAddon addonv1alpha1.ManagedClusterAddOn
	err = singleton.GetClient().Get(ctx, types.NamespacedName{Name: common.AddonName, Namespace: cluster.Name}, &gwAddon)
	if err != nil {
		return nil, err
	}

	endpointType := getClusterEndpointType(ctx, cluster.Name)

	var secret v1.Secret
	err = singleton.GetClient().Get(ctx, types.NamespacedName{Name: common.AddonName, Namespace: cluster.Name}, &secret)
	if err != nil {
		klog.Warningf("Failed getting secret %q/%q: %v", cluster.Name, common.AddonName, err)
		return nil, err
	}

	return convert(&cluster, &gwAddon, endpointType, &secret)
}

func (in *ClusterGateway) List(ctx context.Context, opt *internalversion.ListOptions) (runtime.Object, error) {
	if opt.Watch {
		// TODO: convert watch events from both Secret and ManagedCluster
		return nil, fmt.Errorf("watch not supported")
	}

	list := &ClusterGatewayList{
		Items: []ClusterGateway{},
	}

	var clusters clusterv1.ManagedClusterList
	err := singleton.GetClient().List(ctx, &clusters)
	if err != nil {
		return nil, err
	}

	for _, cluster := range clusters.Items {
		var gwAddon addonv1alpha1.ManagedClusterAddOn
		err := singleton.GetClient().Get(ctx, types.NamespacedName{Name: common.AddonName, Namespace: cluster.Name}, &gwAddon)
		if apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return nil, err
		}

		endpointType := getClusterEndpointType(ctx, cluster.Name)

		var secret v1.Secret
		err = singleton.GetClient().Get(ctx, types.NamespacedName{Name: common.AddonName, Namespace: cluster.Name}, &secret)
		if apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return nil, err
		}

		gw, err := convert(&cluster, &gwAddon, endpointType, &secret)
		if err != nil {
			klog.Warningf("skipping %v: failed converting clustergateway resource", secret.Name)
			continue
		}
		list.Items = append(list.Items, *gw)
	}
	return list, nil
}

func (in *ClusterGateway) ConvertToTable(ctx context.Context, object runtime.Object, tableOptions runtime.Object) (*metav1.Table, error) {
	switch object.(type) {
	case *ClusterGateway:
		return printClusterGateway(object.(*ClusterGateway)), nil
	case *ClusterGatewayList:
		return printClusterGatewayList(object.(*ClusterGatewayList)), nil
	default:
		return nil, fmt.Errorf("unknown type %T", object)
	}
}

func getClusterEndpointType(ctx context.Context, clusterName string) ClusterEndpointType {
	endpointType := ClusterEndpointTypeConst
	if config.ClusterProxyHost != "" {
		var proxyAddon addonv1alpha1.ManagedClusterAddOn
		err := singleton.GetClient().Get(ctx, types.NamespacedName{Name: "cluster-proxy", Namespace: clusterName}, &proxyAddon)
		if err == nil {
			for _, cond := range proxyAddon.Status.Conditions {
				if cond.Type == "Available" && cond.Status == metav1.ConditionTrue {
					endpointType = ClusterEndpointTypeClusterProxy
					break
				}
			}
		}
	}
	return endpointType
}

func getEndpointFromManagedCluster(cluster *clusterv1.ManagedCluster) ([]byte, string, error) {
	if len(cluster.Spec.ManagedClusterClientConfigs) == 0 {
		return nil, "", nil
	}
	cfg := cluster.Spec.ManagedClusterClientConfigs[0]
	return cfg.CABundle, cfg.URL, nil
}

func convert(cluster *clusterv1.ManagedCluster, gwAddon *addonv1alpha1.ManagedClusterAddOn, endpointType ClusterEndpointType, secret *v1.Secret) (*ClusterGateway, error) {
	caData, apiServerEndpoint, err := getEndpointFromManagedCluster(cluster)
	if err != nil {
		return nil, err
	}
	insecure := len(caData) == 0

	c := &ClusterGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name: secret.Name,
		},
		Spec: ClusterGatewaySpec{
			Provider: "",
			Access:   ClusterAccess{},
		},
	}

	// converting endpoint
	var proxyURL *string
	if url, useProxy := gwAddon.Annotations["proxy-url"]; useProxy && len(url) > 0 {
		proxyURL = pointer.String(string(url))
	}
	switch endpointType {
	case ClusterEndpointTypeClusterProxy:
		c.Spec.Access.Endpoint = &ClusterEndpoint{
			Type: endpointType,
		}
	case ClusterEndpointTypeConst:
		fallthrough // backward compatibility
	default:
		if len(apiServerEndpoint) == 0 {
			return nil, errors.New("missing label key: api-endpoint")
		}
		if insecure {
			c.Spec.Access.Endpoint = &ClusterEndpoint{
				Type: endpointType,
				Const: &ClusterEndpointConst{
					Address:  apiServerEndpoint,
					Insecure: &insecure,
					ProxyURL: proxyURL,
				},
			}
		} else {
			c.Spec.Access.Endpoint = &ClusterEndpoint{
				Type: endpointType,
				Const: &ClusterEndpointConst{
					Address:  apiServerEndpoint,
					CABundle: caData,
					ProxyURL: proxyURL,
				},
			}
		}
	}

	// converting credential
	credentialType, ok := secret.Labels[common.LabelKeyClusterCredentialType]
	if !ok {
		if secret.Labels[common.LabelKeyIsManagedServiceAccount] != "true" {
			return nil, apierrors.NewNotFound(schema.GroupResource{
				Group:    config.MetaApiGroupName,
				Resource: config.MetaApiResourceName,
			}, secret.Name)
		}
		credentialType = string(CredentialTypeServiceAccountToken)
	}
	switch CredentialType(credentialType) {
	case CredentialTypeX509Certificate:
		c.Spec.Access.Credential = &ClusterAccessCredential{
			Type: CredentialTypeX509Certificate,
			X509: &X509{
				Certificate: secret.Data[v1.TLSCertKey],
				PrivateKey:  secret.Data[v1.TLSPrivateKeyKey],
			},
		}
	case CredentialTypeServiceAccountToken:
		c.Spec.Access.Credential = &ClusterAccessCredential{
			Type:                CredentialTypeServiceAccountToken,
			ServiceAccountToken: string(secret.Data[v1.ServiceAccountTokenKey]),
		}
	default:
		return nil, fmt.Errorf("unrecognized secret credential type %v", credentialType)
	}

	if utilfeature.DefaultMutableFeatureGate.Enabled(featuregates.HealthinessCheck) {
		if healthyRaw, ok := gwAddon.Annotations[common.AnnotationKeyClusterGatewayStatusHealthy]; ok {
			healthy, err := strconv.ParseBool(healthyRaw)
			if err != nil {
				return nil, fmt.Errorf("unrecogized healthiness status: %v", healthyRaw)
			}
			c.Status.Healthy = healthy
		}
		if healthyReason, ok := gwAddon.Annotations[common.AnnotationKeyClusterGatewayStatusHealthyReason]; ok {
			c.Status.HealthyReason = HealthyReasonType(healthyReason)
		}
	}

	if utilfeature.DefaultMutableFeatureGate.Enabled(featuregates.ClientIdentityPenetration) {
		if proxyConfigRaw, ok := gwAddon.Annotations[AnnotationClusterGatewayProxyConfiguration]; ok {
			proxyConfig := &ClusterGatewayProxyConfiguration{}
			if err := yaml.Unmarshal([]byte(proxyConfigRaw), proxyConfig); err == nil {
				for _, rule := range proxyConfig.Spec.Rules {
					rule.Source.Cluster = pointer.String(c.Name)
				}
				c.Spec.ProxyConfig = proxyConfig
			}
		}
	}

	return c, nil
}

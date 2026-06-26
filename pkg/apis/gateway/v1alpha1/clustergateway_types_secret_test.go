package v1alpha1

import (
	"context"
	"testing"

	"github.com/kluster-manager/cluster-gateway/pkg/common"
	"github.com/kluster-manager/cluster-gateway/pkg/util/singleton"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/pointer"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testClusterName = "bar"
	testCAData      = "caData"
	testCertData    = "certData"
	testKeyData     = "keyData"
	testToken       = "token"
	testEndpoint    = "https://localhost:443"
)

var (
	x509Labels  = map[string]string{common.LabelKeyClusterCredentialType: string(CredentialTypeX509Certificate)}
	tokenLabels = map[string]string{common.LabelKeyClusterCredentialType: string(CredentialTypeServiceAccountToken)}
	x509Data    = map[string][]byte{
		corev1.TLSCertKey:       []byte(testCertData),
		corev1.TLSPrivateKeyKey: []byte(testKeyData),
	}
	tokenData = map[string][]byte{
		corev1.ServiceAccountTokenKey: []byte(testToken),
	}
)

// managedCluster builds a ManagedCluster whose first client config carries the
// given API server endpoint and CA bundle (the source convert() reads the
// endpoint from). A nil caBundle yields an insecure const endpoint.
func managedCluster(name, endpoint string, caBundle []byte) *clusterv1.ManagedCluster {
	cluster := &clusterv1.ManagedCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	if endpoint != "" {
		cluster.Spec.ManagedClusterClientConfigs = []clusterv1.ClientConfig{
			{URL: endpoint, CABundle: caBundle},
		}
	}
	return cluster
}

func gatewayAddon(namespace string, annotations map[string]string) *addonv1alpha1.ManagedClusterAddOn {
	return &addonv1alpha1.ManagedClusterAddOn{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   namespace,
			Name:        common.AddonName,
			Annotations: annotations,
		},
	}
}

func credentialSecret(namespace string, labels map[string]string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      common.AddonName,
			Labels:    labels,
		},
		Data: data,
	}
}

func x509Credential() *ClusterAccessCredential {
	return &ClusterAccessCredential{
		Type: CredentialTypeX509Certificate,
		X509: &X509{
			Certificate: []byte(testCertData),
			PrivateKey:  []byte(testKeyData),
		},
	}
}

func TestConvert(t *testing.T) {
	cases := []struct {
		name            string
		cluster         *clusterv1.ManagedCluster
		gwAddon         *addonv1alpha1.ManagedClusterAddOn
		endpointType    ClusterEndpointType
		secret          *corev1.Secret
		expectedFailure bool
		expected        *ClusterGateway
	}{
		{
			name:         "x509 certificate, const endpoint",
			cluster:      managedCluster(testClusterName, testEndpoint, []byte(testCAData)),
			gwAddon:      gatewayAddon(testClusterName, nil),
			endpointType: ClusterEndpointTypeConst,
			secret:       credentialSecret(testClusterName, x509Labels, x509Data),
			expected: &ClusterGateway{
				ObjectMeta: metav1.ObjectMeta{Name: testClusterName},
				Spec: ClusterGatewaySpec{
					Access: ClusterAccess{
						Credential: x509Credential(),
						Endpoint: &ClusterEndpoint{
							Type: ClusterEndpointTypeConst,
							Const: &ClusterEndpointConst{
								Address:  testEndpoint,
								CABundle: []byte(testCAData),
							},
						},
					},
				},
			},
		},
		{
			name:         "service-account token, const endpoint",
			cluster:      managedCluster(testClusterName, testEndpoint, []byte(testCAData)),
			gwAddon:      gatewayAddon(testClusterName, nil),
			endpointType: ClusterEndpointTypeConst,
			secret:       credentialSecret(testClusterName, tokenLabels, tokenData),
			expected: &ClusterGateway{
				ObjectMeta: metav1.ObjectMeta{Name: testClusterName},
				Spec: ClusterGatewaySpec{
					Access: ClusterAccess{
						Credential: &ClusterAccessCredential{
							Type:                CredentialTypeServiceAccountToken,
							ServiceAccountToken: testToken,
						},
						Endpoint: &ClusterEndpoint{
							Type: ClusterEndpointTypeConst,
							Const: &ClusterEndpointConst{
								Address:  testEndpoint,
								CABundle: []byte(testCAData),
							},
						},
					},
				},
			},
		},
		{
			name:         "cluster-proxy endpoint ignores the cluster client config",
			cluster:      managedCluster(testClusterName, "", nil),
			gwAddon:      gatewayAddon(testClusterName, nil),
			endpointType: ClusterEndpointTypeClusterProxy,
			secret:       credentialSecret(testClusterName, x509Labels, x509Data),
			expected: &ClusterGateway{
				ObjectMeta: metav1.ObjectMeta{Name: testClusterName},
				Spec: ClusterGatewaySpec{
					Access: ClusterAccess{
						Credential: x509Credential(),
						Endpoint: &ClusterEndpoint{
							Type: ClusterEndpointTypeClusterProxy,
						},
					},
				},
			},
		},
		{
			name:         "missing CA bundle yields an insecure const endpoint",
			cluster:      managedCluster(testClusterName, testEndpoint, nil),
			gwAddon:      gatewayAddon(testClusterName, nil),
			endpointType: ClusterEndpointTypeConst,
			secret:       credentialSecret(testClusterName, x509Labels, x509Data),
			expected: &ClusterGateway{
				ObjectMeta: metav1.ObjectMeta{Name: testClusterName},
				Spec: ClusterGatewaySpec{
					Access: ClusterAccess{
						Credential: x509Credential(),
						Endpoint: &ClusterEndpoint{
							Type: ClusterEndpointTypeConst,
							Const: &ClusterEndpointConst{
								Address:  testEndpoint,
								Insecure: pointer.Bool(true),
							},
						},
					},
				},
			},
		},
		{
			name:         "proxy-url addon annotation is propagated",
			cluster:      managedCluster(testClusterName, testEndpoint, []byte(testCAData)),
			gwAddon:      gatewayAddon(testClusterName, map[string]string{"proxy-url": "socks5://localhost:1080"}),
			endpointType: ClusterEndpointTypeConst,
			secret:       credentialSecret(testClusterName, x509Labels, x509Data),
			expected: &ClusterGateway{
				ObjectMeta: metav1.ObjectMeta{Name: testClusterName},
				Spec: ClusterGatewaySpec{
					Access: ClusterAccess{
						Credential: x509Credential(),
						Endpoint: &ClusterEndpoint{
							Type: ClusterEndpointTypeConst,
							Const: &ClusterEndpointConst{
								Address:  testEndpoint,
								CABundle: []byte(testCAData),
								ProxyURL: pointer.String("socks5://localhost:1080"),
							},
						},
					},
				},
			},
		},
		{
			name:    "healthiness annotations on the addon populate status",
			cluster: managedCluster(testClusterName, testEndpoint, []byte(testCAData)),
			gwAddon: gatewayAddon(testClusterName, map[string]string{
				common.AnnotationKeyClusterGatewayStatusHealthy:       "true",
				common.AnnotationKeyClusterGatewayStatusHealthyReason: "MyReason",
			}),
			endpointType: ClusterEndpointTypeConst,
			secret:       credentialSecret(testClusterName, x509Labels, x509Data),
			expected: &ClusterGateway{
				ObjectMeta: metav1.ObjectMeta{Name: testClusterName},
				Spec: ClusterGatewaySpec{
					Access: ClusterAccess{
						Credential: x509Credential(),
						Endpoint: &ClusterEndpoint{
							Type: ClusterEndpointTypeConst,
							Const: &ClusterEndpointConst{
								Address:  testEndpoint,
								CABundle: []byte(testCAData),
							},
						},
					},
				},
				Status: ClusterGatewayStatus{
					Healthy:       true,
					HealthyReason: "MyReason",
				},
			},
		},
		{
			name:            "missing credential type label fails",
			cluster:         managedCluster(testClusterName, testEndpoint, []byte(testCAData)),
			gwAddon:         gatewayAddon(testClusterName, nil),
			endpointType:    ClusterEndpointTypeConst,
			secret:          credentialSecret(testClusterName, nil, nil),
			expectedFailure: true,
		},
		{
			name:            "const endpoint without an api server address fails",
			cluster:         managedCluster(testClusterName, "", nil),
			gwAddon:         gatewayAddon(testClusterName, nil),
			endpointType:    ClusterEndpointTypeConst,
			secret:          credentialSecret(testClusterName, x509Labels, x509Data),
			expectedFailure: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gw, err := convert(c.cluster, c.gwAddon, c.endpointType, c.secret)
			if c.expectedFailure {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.expected, gw)
		})
	}
}

func TestListHybridClusterGateway(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.Install(scheme))
	require.NoError(t, addonv1alpha1.Install(scheme))

	objs := []client.Object{
		// cluster-a and cluster-b are fully wired (cluster + gateway addon +
		// credential secret) and should appear in the list.
		managedCluster("cluster-a", testEndpoint, []byte(testCAData)),
		gatewayAddon("cluster-a", nil),
		credentialSecret("cluster-a", x509Labels, x509Data),
		managedCluster("cluster-b", testEndpoint, []byte(testCAData)),
		gatewayAddon("cluster-b", nil),
		credentialSecret("cluster-b", x509Labels, x509Data),
		// cluster-c has no gateway addon -> skipped.
		managedCluster("cluster-c", testEndpoint, []byte(testCAData)),
		// cluster-d has the addon but no credential secret -> skipped.
		managedCluster("cluster-d", testEndpoint, []byte(testCAData)),
		gatewayAddon("cluster-d", nil),
	}

	fakeClient := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	singleton.SetClient(fakeClient)

	storage := &ClusterGateway{}
	out, err := storage.List(context.TODO(), &internalversion.ListOptions{})
	require.NoError(t, err)

	gateways := out.(*ClusterGatewayList)
	actualNames := sets.NewString()
	for _, gw := range gateways.Items {
		actualNames.Insert(gw.Name)
	}
	assert.Equal(t, sets.NewString("cluster-a", "cluster-b"), actualNames)
}

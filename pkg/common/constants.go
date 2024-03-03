package common

import "github.com/kluster-manager/cluster-gateway/pkg/config"

const (
	AddonName = "cluster-gateway"
)

const (
	LabelKeyOpenClusterManagementAddon = "config.gateway.open-cluster-management.io/addon-name"
)

const (
	ClusterGatewayConfigurationCRDName = "clustergatewayconfigurations.config.gateway.open-cluster-management.io"
	ClusterGatewayConfigurationCRName  = "cluster-gateway"
)

const (
	InstallNamespace = "open-cluster-management-cluster-gateway"
)

const (
	ClusterGatewayAPIServiceName = "v1alpha1.gateway.open-cluster-management.io"
)

const (
	// LabelKeyClusterCredentialType describes the credential type in object label field
	LabelKeyClusterCredentialType = config.MetaApiGroupName + "/cluster-credential-type"
	// LabelKeyClusterEndpointType describes the endpoint type.
	LabelKeyClusterEndpointType = config.MetaApiGroupName + "/cluster-endpoint-type"
	// LabelKeyManagedServiceAccount describes the label used by managed service account secret
	LabelKeyManagedServiceAccount = "authentication.open-cluster-management.io/is-managed-serviceaccount"
)

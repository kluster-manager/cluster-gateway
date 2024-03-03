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
	// LabelKeyIsManagedServiceaccount describes the credential is a manageed service account token
	LabelKeyIsManagedServiceAccount                = "authentication.open-cluster-management.io/is-managed-serviceaccount"
	AnnotationKeyClusterGatewayStatusHealthy       = "status.gateway.open-cluster-management.io/healthy"
	AnnotationKeyClusterGatewayStatusHealthyReason = "status.gateway.open-cluster-management.io/healthy-reason"
)

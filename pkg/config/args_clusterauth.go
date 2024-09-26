package config

import (
	"github.com/spf13/pflag"
)

var ClusterAuthNamespace string

func AddClusterAuthNamespaceFlags(set *pflag.FlagSet) {
	set.StringVar(&ClusterAuthNamespace, "cluster-auth-namespace", "open-cluster-management-cluster-auth",
		"Namespace used to create service accounts for cluster-auth addon")
}

/*
Copyright 2023 The KubeVela Authors.

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

package v1alpha1

import (
	"context"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/strings/slices"
	ocmclusterv1 "open-cluster-management.io/api/cluster/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kluster-manager/cluster-gateway/pkg/common"
	"github.com/kluster-manager/cluster-gateway/pkg/config"
)

// VirtualClusterClient client for reading cluster information
// +kubebuilder:object:generate=false
type VirtualClusterClient interface {
	Get(ctx context.Context, name string) (*VirtualCluster, error)
	List(ctx context.Context, options ...client.ListOption) (*VirtualClusterList, error)
}

type virtualClusterClient struct {
	client.Client
}

// NewVirtualClusterClient create a client for accessing cluster
func NewVirtualClusterClient(cli client.Client) VirtualClusterClient {
	return &virtualClusterClient{Client: cli}
}

func (c *virtualClusterClient) Get(ctx context.Context, name string) (*VirtualCluster, error) {
	if name == ClusterLocalName {
		return NewLocalCluster(), nil
	}
	key := types.NamespacedName{Name: name}
	var cluster *VirtualCluster
	managedCluster := &ocmclusterv1.ManagedCluster{}
	err := c.Client.Get(ctx, key, managedCluster)
	var managedClusterErr error
	if err == nil {
		if cluster, managedClusterErr = NewClusterFromManagedCluster(managedCluster); managedClusterErr == nil {
			return cluster, nil
		}
	}

	if err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) && !runtime.IsNotRegisteredError(err) {
		managedClusterErr = err
	}
	if managedClusterErr != nil {
		return nil, managedClusterErr
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{
		Group:    config.MetaApiGroupName,
		Resource: "virtualclusters",
	}, name)
}

func (c *virtualClusterClient) List(ctx context.Context, options ...client.ListOption) (*VirtualClusterList, error) {
	opts := &client.ListOptions{}
	for _, opt := range options {
		opt.ApplyToList(opts)
	}
	local := NewLocalCluster()
	clusters := &VirtualClusterList{Items: []VirtualCluster{*local}}

	managedClusters := &ocmclusterv1.ManagedClusterList{}
	err := c.Client.List(ctx, managedClusters, virtualClusterSelector{selector: opts.LabelSelector, requireCredentialType: false})
	if err != nil && !meta.IsNoMatchError(err) && !runtime.IsNotRegisteredError(err) {
		return nil, err
	}
	for _, managedCluster := range managedClusters.Items {
		if !clusters.HasCluster(managedCluster.Name) {
			if cluster, err := NewClusterFromManagedCluster(managedCluster.DeepCopy()); err == nil {
				clusters.Items = append(clusters.Items, *cluster)
			}
		}
	}

	// filter clusters
	var items []VirtualCluster
	for _, cluster := range clusters.Items {
		if opts.LabelSelector == nil || opts.LabelSelector.Matches(labels.Set(cluster.GetLabels())) {
			items = append(items, cluster)
		}
	}
	clusters.Items = items

	// sort clusters
	sort.Slice(clusters.Items, func(i, j int) bool {
		if clusters.Items[i].Name == ClusterLocalName {
			return true
		} else if clusters.Items[j].Name == ClusterLocalName {
			return false
		} else {
			return clusters.Items[i].CreationTimestamp.After(clusters.Items[j].CreationTimestamp.Time)
		}
	})
	return clusters, nil
}

// virtualClusterSelector filters the list/delete operation of cluster list
type virtualClusterSelector struct {
	selector              labels.Selector
	requireCredentialType bool
	namespace             string
}

// ApplyToList applies this configuration to the given list options.
func (m virtualClusterSelector) ApplyToList(opts *client.ListOptions) {
	opts.LabelSelector = labels.NewSelector()
	if m.selector != nil {
		requirements, _ := m.selector.Requirements()
		for _, r := range requirements {
			if !slices.Contains([]string{LabelClusterControlPlane}, r.Key()) {
				opts.LabelSelector = opts.LabelSelector.Add(r)
			}
		}
	}
	if m.requireCredentialType {
		r, _ := labels.NewRequirement(common.LabelKeyClusterCredentialType, selection.Exists, nil)
		opts.LabelSelector = opts.LabelSelector.Add(*r)
	}
	opts.Namespace = m.namespace
}

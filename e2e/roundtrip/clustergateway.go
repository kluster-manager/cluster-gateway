package roundtrip

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clusterv1 "open-cluster-management.io/api/cluster/v1"

	"github.com/kluster-manager/cluster-gateway/e2e/framework"
	multicluster "github.com/kluster-manager/cluster-gateway/pkg/apis/gateway/transport"
	gatewayv1alpha1 "github.com/kluster-manager/cluster-gateway/pkg/apis/gateway/v1alpha1"
)

const (
	roundtripTestBasename = "roundtrip"
)

var _ = Describe("Basic RoundTrip Test", func() {
	f := framework.NewE2EFramework(roundtripTestBasename)

	It("ClusterGateway in the API discovery",
		func() {
			By("Discovering ClusterGateway")
			nativeClient := f.HubNativeClient()
			resources, err := nativeClient.Discovery().
				ServerResourcesForGroupVersion("gateway.open-cluster-management.io/v1alpha1")
			Expect(err).NotTo(HaveOccurred())
			apiFound := false
			for _, resource := range resources.APIResources {
				if resource.Kind == "ClusterGateway" {
					apiFound = true
				}
			}
			if !apiFound {
				Fail(`Api ClusterGateway not found`)
			}
		})

	It("ManagedCluster present",
		func() {
			By("Getting ManagedCluster")
			if f.IsOCMInstalled() {
				runtimeClient := f.HubRuntimeClient()
				cluster := &clusterv1.ManagedCluster{}
				err := runtimeClient.Get(context.TODO(), types.NamespacedName{
					Name: f.TestClusterName(),
				}, cluster)
				Expect(err).NotTo(HaveOccurred())
			}
		})

	It("ClusterGateway can be read via GET",
		func() {
			By("Getting ClusterGateway")
			runtimeClient := f.HubRuntimeClient()
			clusterGateway := &gatewayv1alpha1.ClusterGateway{}
			err := runtimeClient.Get(context.TODO(), types.NamespacedName{
				Name: f.TestClusterName(),
			}, clusterGateway)
			Expect(err).NotTo(HaveOccurred())
		})

	It("ClusterGateway can be read via LIST",
		func() {
			By("Getting ClusterGateway")
			runtimeClient := f.HubRuntimeClient()
			clusterGatewayList := &gatewayv1alpha1.ClusterGatewayList{}
			err := runtimeClient.List(context.TODO(), clusterGatewayList)
			Expect(err).NotTo(HaveOccurred())
			clusterFound := false
			for _, clusterGateway := range clusterGatewayList.Items {
				if clusterGateway.Name == f.TestClusterName() {
					clusterFound = true
				}
			}
			if !clusterFound {
				Fail(`ClusterGateway not found`)
			}
		})

	It("ClusterGateway healthiness can be manipulated",
		func() {
			By("get healthiness")
			gw, err := f.HubGatewayClient().
				GatewayV1alpha1().
				ClusterGateways().
				GetHealthiness(context.TODO(), f.TestClusterName(), metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(gw).ShouldNot(BeNil())
			Expect(gw.Status.Healthy).To(BeFalse())
			By("update healthiness")
			gw.Status.Healthy = true
			gw.Status.HealthyReason = gatewayv1alpha1.HealthyReasonTypeConnectionTimeout
			updated, err := f.HubGatewayClient().
				GatewayV1alpha1().
				ClusterGateways().
				UpdateHealthiness(context.TODO(), gw, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(updated).NotTo(BeNil())
			Expect(updated.Status.Healthy).To(BeTrue())
			Expect(updated.Status.HealthyReason).To(Equal(gatewayv1alpha1.HealthyReasonTypeConnectionTimeout))
		})

	It("Probing cluster health (raw)",
		func() {
			resp, err := f.HubNativeClient().Discovery().
				RESTClient().
				Get().
				AbsPath(
					"apis/gateway.open-cluster-management.io/v1alpha1/clustergateways",
					f.TestClusterName(),
					"proxy",
					"healthz",
				).DoRaw(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			Expect(string(resp)).To(Equal("ok"))
		})

	It("Probing cluster health (context)",
		func() {
			cfg := f.HubRESTConfig()
			cfg.WrapTransport = multicluster.NewClusterGatewayRoundTripper
			multiClusterClient, err := kubernetes.NewForConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
			resp, err := multiClusterClient.RESTClient().
				Get().
				AbsPath("healthz").
				DoRaw(multicluster.WithMultiClusterContext(context.TODO(), f.TestClusterName()))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(resp)).To(Equal("ok"))
		})

})

package v1alpha1

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/pkg/errors"
	"google.golang.org/grpc"
	grpccredentials "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	k8snet "k8s.io/apimachinery/pkg/util/net"
	restclient "k8s.io/client-go/rest"
	k8stransport "k8s.io/client-go/transport"
	konnectivity "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"

	"github.com/kluster-manager/cluster-gateway/pkg/config"
)

// DialerGetter returns a dialer that creates a konnectivity tunnel on demand,
// scoped to context.Background() so the connection it backs can be pooled.
var DialerGetter = func(_ context.Context) (k8snet.DialFunc, error) {
	proxyAddress := net.JoinHostPort(config.ClusterProxyHost, strconv.Itoa(config.ClusterProxyPort))
	return func(ctx context.Context, _, addr string) (net.Conn, error) {
		tlsCfg, err := util.GetClientTLSConfig(
			config.ClusterProxyCAFile,
			config.ClusterProxyCertFile,
			config.ClusterProxyKeyFile,
			config.ClusterProxyHost,
			nil)
		if err != nil {
			return nil, err
		}
		dialerTunnel, err := konnectivity.CreateSingleUseGrpcTunnel(
			context.Background(),
			proxyAddress,
			grpc.WithTransportCredentials(grpccredentials.NewTLS(tlsCfg)),
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time: time.Second * 5,
			}),
		)
		if err != nil {
			return nil, err
		}
		return dialerTunnel.DialContext(ctx, "tcp", addr)
	}, nil
}

var (
	clusterProxyDialHolderOnce sync.Once
	clusterProxyDialHolderInst *k8stransport.DialHolder
	clusterProxyDialHolderErr  error
)

// ClusterProxyDialHolder returns a process-wide stable DialHolder so client-go's
// transport cache can reuse one pooled http.Transport per cluster credential.
func ClusterProxyDialHolder() (*k8stransport.DialHolder, error) {
	clusterProxyDialHolderOnce.Do(func() {
		dial, err := DialerGetter(context.Background())
		if err != nil {
			clusterProxyDialHolderErr = err
			return
		}
		clusterProxyDialHolderInst = &k8stransport.DialHolder{Dial: dial}
	})
	return clusterProxyDialHolderInst, clusterProxyDialHolderErr
}

func NewConfigFromCluster(ctx context.Context, c *ClusterGateway) (*restclient.Config, error) {
	cfg := &restclient.Config{
		Timeout: time.Second * 40,
	}
	// setting up endpoint
	switch c.Spec.Access.Endpoint.Type {
	case ClusterEndpointTypeConst:
		cfg.Host = c.Spec.Access.Endpoint.Const.Address
		cfg.CAData = c.Spec.Access.Endpoint.Const.CABundle
		if c.Spec.Access.Endpoint.Const.Insecure != nil && *c.Spec.Access.Endpoint.Const.Insecure {
			cfg.TLSClientConfig = restclient.TLSClientConfig{Insecure: true}
		}
		u, err := url.Parse(c.Spec.Access.Endpoint.Const.Address)
		if err != nil {
			return nil, err
		}

		const missingPort = "missing port in address"
		host, _, err := net.SplitHostPort(u.Host)
		if err != nil {
			addrErr, ok := err.(*net.AddrError)
			if !ok {
				return nil, err
			}
			if addrErr.Err != missingPort {
				return nil, err
			}
			host = u.Host
		}
		cfg.ServerName = host // apiserver may listen on SNI cert

		if c.Spec.Access.Endpoint.Const.ProxyURL != nil {
			_url, _err := url.Parse(*c.Spec.Access.Endpoint.Const.ProxyURL)
			if _err != nil {
				return nil, _err
			}
			cfg.Proxy = http.ProxyURL(_url)
		}
	case ClusterEndpointTypeClusterProxy:
		cfg.Host = c.Name // the same as the cluster name
		cfg.Insecure = true
		cfg.CAData = nil
		dail, err := DialerGetter(ctx)
		if err != nil {
			return nil, err
		}
		cfg.Dial = dail
	}
	// setting up credentials
	switch c.Spec.Access.Credential.Type {
	case CredentialTypeServiceAccountToken:
		cfg.BearerToken = c.Spec.Access.Credential.ServiceAccountToken
	case CredentialTypeX509Certificate:
		cfg.CertData = c.Spec.Access.Credential.X509.Certificate
		cfg.KeyData = c.Spec.Access.Credential.X509.PrivateKey
	}
	return cfg, nil
}

func GetEndpointURL(c *ClusterGateway) (*url.URL, error) {
	switch c.Spec.Access.Endpoint.Type {
	case ClusterEndpointTypeConst:
		urlAddr, err := url.Parse(c.Spec.Access.Endpoint.Const.Address)
		if err != nil {
			return nil, errors.Wrapf(err, "failed parsing url from cluster %s invalid value %s",
				c.Name, c.Spec.Access.Endpoint.Const.Address)
		}
		return urlAddr, nil
	case ClusterEndpointTypeClusterProxy:
		return &url.URL{
			Scheme: "https",
			Host:   c.Name,
		}, nil
	default:
		return nil, errors.New("unsupported cluster gateway endpoint type")
	}
}

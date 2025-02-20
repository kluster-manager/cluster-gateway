package v1alpha1

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/pkg/errors"
	"google.golang.org/grpc"
	grpccredentials "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	k8snet "k8s.io/apimachinery/pkg/util/net"
	restclient "k8s.io/client-go/rest"
	konnectivity "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"

	"github.com/kluster-manager/cluster-gateway/pkg/config"
)

type tlsCacheKey struct {
	caData     string
	certData   string
	keyData    string
	serverName string
}

var mu sync.Mutex
var tlsCache = map[tlsCacheKey]*tls.Config{}

func GetClientTLSConfig(caFile, certFile, keyFile, serverName string) (*tls.Config, error) {
	mu.Lock()
	defer mu.Unlock()

	caData, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	certData, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	keyData, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}

	key := tlsCacheKey{
		caData:     string(caData),
		certData:   string(certData),
		keyData:    string(keyData),
		serverName: serverName,
	}
	cache, found := tlsCache[key]
	if found {
		return cache, nil
	}

	tlsCfg, err := util.GetClientTLSConfig(
		config.ClusterProxyCAFile,
		config.ClusterProxyCertFile,
		config.ClusterProxyKeyFile,
		config.ClusterProxyHost,
		nil)
	if err != nil {
		return nil, err
	}
	tlsCache[key] = tlsCfg
	return tlsCfg, nil
}

var DialerGetter = func(ctx context.Context) (k8snet.DialFunc, error) {
	tlsCfg, err := GetClientTLSConfig(
		config.ClusterProxyCAFile,
		config.ClusterProxyCertFile,
		config.ClusterProxyKeyFile,
		config.ClusterProxyHost)
	if err != nil {
		return nil, err
	}
	dialerTunnel, err := konnectivity.CreateSingleUseGrpcTunnelWithContext(
		context.TODO(),
		ctx,
		net.JoinHostPort(config.ClusterProxyHost, strconv.Itoa(config.ClusterProxyPort)),
		grpc.WithTransportCredentials(grpccredentials.NewTLS(tlsCfg)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time: time.Second * 10,
		}),
		grpc.WithSharedWriteBuffer(true),
		grpc.WithReadBufferSize(0),
	)
	if err != nil {
		return nil, err
	}
	return dialerTunnel.DialContext, nil
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
		dial, err := DialerGetter(ctx)
		if err != nil {
			return nil, err
		}
		cfg.Dial = dial
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

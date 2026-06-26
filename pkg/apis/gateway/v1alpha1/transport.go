package v1alpha1

import (
	"context"
	"crypto/sha256"
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
	k8stransport "k8s.io/client-go/transport"
	konnectivity "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"

	"github.com/kluster-manager/cluster-gateway/pkg/config"
)

// DialerGetter returns a dialer that creates a konnectivity tunnel on demand,
// scoped to context.Background() so the connection it backs can be pooled.
//
// The client TLS material is read once here (when the dialer is built) rather
// than on every dial, so it does not sit on the connection hot path. The client
// cert/key are reloaded lazily through tls.Config.GetClientCertificate when the
// files change, so a rotation of the cluster-proxy client cert/key is picked up
// on the next tunnel without a process restart. (Rotation of the CA bundle still
// requires a restart.)
var DialerGetter = func(_ context.Context) (k8snet.DialFunc, error) {
	proxyAddress := net.JoinHostPort(config.ClusterProxyHost, strconv.Itoa(config.ClusterProxyPort))
	tlsCfg, err := util.GetClientTLSConfig(
		config.ClusterProxyCAFile,
		config.ClusterProxyCertFile,
		config.ClusterProxyKeyFile,
		config.ClusterProxyHost,
		nil)
	if err != nil {
		return nil, err
	}
	// Reload the client cert/key on rotation instead of pinning the copy read
	// above for the lifetime of the process.
	if config.ClusterProxyCertFile != "" || config.ClusterProxyKeyFile != "" {
		reloader := &reloadingClientCert{certFile: config.ClusterProxyCertFile, keyFile: config.ClusterProxyKeyFile}
		tlsCfg.Certificates = nil
		tlsCfg.GetClientCertificate = reloader.getClientCertificate
	}
	transportCreds := grpccredentials.NewTLS(tlsCfg)
	return func(ctx context.Context, _, addr string) (net.Conn, error) {
		dialerTunnel, err := konnectivity.CreateSingleUseGrpcTunnel(
			context.Background(),
			proxyAddress,
			grpc.WithTransportCredentials(transportCreds),
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

// reloadingClientCert loads the cluster-proxy client cert/key from disk and
// re-reads them when the files change, so cert rotation does not require a
// process restart. tls.Config calls getClientCertificate once per handshake.
type reloadingClientCert struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	modCert time.Time
	modKey  time.Time
}

func (r *reloadingClientCert) getClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		return nil, err
	}
	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		return nil, err
	}
	if r.cert == nil || certInfo.ModTime().After(r.modCert) || keyInfo.ModTime().After(r.modKey) {
		cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
		if err != nil {
			return nil, err
		}
		r.cert = &cert
		r.modCert = certInfo.ModTime()
		r.modKey = keyInfo.ModTime()
	}
	return r.cert, nil
}

var (
	clusterProxyDialHolderMu   sync.Mutex
	clusterProxyDialHolderInst *k8stransport.DialHolder
)

// ClusterProxyDialHolder returns a process-wide stable DialHolder so client-go's
// transport cache can reuse one pooled http.Transport per cluster credential.
//
// The holder is built lazily and only memoized on success, so a transient
// failure (e.g. the cluster-proxy client certs not yet mounted at startup) is
// retried on the next call instead of bricking cluster-proxy until a restart.
func ClusterProxyDialHolder() (*k8stransport.DialHolder, error) {
	clusterProxyDialHolderMu.Lock()
	defer clusterProxyDialHolderMu.Unlock()
	if clusterProxyDialHolderInst != nil {
		return clusterProxyDialHolderInst, nil
	}
	dial, err := DialerGetter(context.Background())
	if err != nil {
		return nil, err
	}
	clusterProxyDialHolderInst = &k8stransport.DialHolder{Dial: dial}
	return clusterProxyDialHolderInst, nil
}

type clusterProxyUpgradeEntry struct {
	credKey   string
	transport *http.Transport
}

var (
	clusterProxyUpgradeMu         sync.Mutex
	clusterProxyUpgradeTransports = map[string]*clusterProxyUpgradeEntry{}
)

// ClusterProxyUpgradeTransport returns a pooled upgrade (SPDY) transport for the
// given cluster, so exec/attach/port-forward requests reuse one http.Transport
// per cluster instead of allocating a fresh one per request. The shared
// cluster-proxy DialHolder dial routes each connection to the right cluster by
// address. The cache is keyed by cluster name and is bounded by the number of
// clusters: when a cluster's credential rotates, the stale transport's idle
// connections are dropped and it is replaced rather than accumulated.
func ClusterProxyUpgradeTransport(clusterName string, transportCfg *k8stransport.Config, dial k8snet.DialFunc) (*http.Transport, error) {
	credKey := clusterProxyUpgradeKey(transportCfg)

	clusterProxyUpgradeMu.Lock()
	defer clusterProxyUpgradeMu.Unlock()
	if entry, ok := clusterProxyUpgradeTransports[clusterName]; ok {
		if entry.credKey == credKey {
			return entry.transport, nil
		}
		// Credential rotated: drop the stale transport's pooled connections
		// before replacing it so we do not leak idle tunnels.
		entry.transport.CloseIdleConnections()
	}

	tlsConfig, err := k8stransport.TLSConfigFor(transportCfg)
	if err != nil {
		return nil, err
	}
	t := k8snet.SetOldTransportDefaults(&http.Transport{
		TLSClientConfig: tlsConfig,
		DialContext:     dial,
	})
	clusterProxyUpgradeTransports[clusterName] = &clusterProxyUpgradeEntry{credKey: credKey, transport: t}
	return t, nil
}

// clusterProxyUpgradeKey is a cheap fingerprint of the TLS credential material,
// used only to detect when a cluster's credential has rotated. It is a private
// hash, not a reimplementation of client-go's transport-cache key.
func clusterProxyUpgradeKey(c *k8stransport.Config) string {
	h := sha256.New()
	writeField := func(b []byte) {
		_, _ = h.Write(b)
		_, _ = h.Write([]byte{0})
	}
	writeField([]byte(strconv.FormatBool(c.TLS.Insecure)))
	writeField([]byte(c.TLS.ServerName))
	writeField(c.TLS.CAData)
	writeField(c.TLS.CertData)
	writeField(c.TLS.KeyData)
	writeField([]byte(c.TLS.CertFile))
	writeField([]byte(c.TLS.KeyFile))
	return string(h.Sum(nil))
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
		// Reuse the process-wide pooled dialer instead of building one (and
		// reading the client TLS material from disk) on every request. ServeHTTP
		// injects the same shared DialHolder, so a per-request dialer here would
		// only be built and discarded; routing every consumer of this config
		// through the pooled holder also keeps connection pooling from depending
		// on which caller built the transport.
		holder, err := ClusterProxyDialHolder()
		if err != nil {
			return nil, err
		}
		cfg.Dial = holder.Dial
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

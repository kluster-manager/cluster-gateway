package v1alpha1

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
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

	"github.com/kluster-manager/cluster-gateway/pkg/config"
)

// clusterProxyTLSReloadInterval bounds how often the cluster-proxy TLS material
// is re-read from disk on the dial path.
const clusterProxyTLSReloadInterval = 30 * time.Second

// DialerGetter returns a dialer that creates a konnectivity tunnel on demand,
// scoped to context.Background() so the connection it backs can be pooled.
//
// The client cert/key and the CA bundle are reloaded from disk by content (not
// mtime), at most once per clusterProxyTLSReloadInterval, so a rotation of any
// of them is picked up on the next tunnel without a process restart, while the
// read stays off the per-handshake hot path. A transient read error falls back
// to the last good material so an in-flight rotation does not break dials.
var DialerGetter = func(_ context.Context) (k8snet.DialFunc, error) {
	proxyAddress := net.JoinHostPort(config.ClusterProxyHost, strconv.Itoa(config.ClusterProxyPort))
	reloader, err := newReloadingTLS(
		config.ClusterProxyCAFile,
		config.ClusterProxyCertFile,
		config.ClusterProxyKeyFile,
		config.ClusterProxyHost,
	)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, _, addr string) (net.Conn, error) {
		dialerTunnel, err := konnectivity.CreateSingleUseGrpcTunnel(
			context.Background(),
			proxyAddress,
			grpc.WithTransportCredentials(grpccredentials.NewTLS(reloader.tlsConfig())),
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

// reloadingTLS reloads the cluster-proxy CA bundle and client cert/key from disk
// and hands out a fresh *tls.Config for each new konnectivity tunnel, so both
// client-cert and CA rotation are picked up without a process restart. It reads
// the files at most once per reload interval and compares by content, and keeps
// the last good material if a re-read transiently fails (e.g. the brief window
// while a Kubernetes secret's ..data symlink is swapped). It mirrors the
// semantics of client-go's dynamicClientCert/cachingCertificateLoader for a path
// that feeds gRPC transport credentials rather than an http.Transport.
type reloadingTLS struct {
	caFile, certFile, keyFile, serverName string

	mu        sync.Mutex
	lastCheck time.Time
	caPool    *x509.CertPool
	caData    []byte
	cert      *tls.Certificate
	// onRotate, if set, is invoked (with mu held) when the cert or CA changes
	// after the initial load — used to drop pooled connections on rotation.
	onRotate func()
}

func newReloadingTLS(caFile, certFile, keyFile, serverName string) (*reloadingTLS, error) {
	r := &reloadingTLS{caFile: caFile, certFile: certFile, keyFile: keyFile, serverName: serverName}
	// Load once up front so a misconfiguration fails fast at dialer-build time
	// rather than on the first proxied request.
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.reloadLocked(); err != nil {
		return nil, err
	}
	return r, nil
}

// tlsConfig returns a fresh *tls.Config carrying the current CA pool and client
// certificate, refreshing them from disk if the reload interval has elapsed.
func (r *reloadingTLS) tlsConfig() *tls.Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.lastCheck) >= clusterProxyTLSReloadInterval {
		// Best effort: reloadLocked keeps the last good material on error.
		_ = r.reloadLocked()
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: r.serverName,
		RootCAs:    r.caPool,
	}
	if r.cert != nil {
		cfg.Certificates = []tls.Certificate{*r.cert}
	}
	return cfg
}

// reloadLocked re-reads the CA and cert/key files. Callers hold r.mu. On a read
// error it returns the error but leaves the previously loaded material in place
// (when any) so transient failures do not break dials; the initial load (when
// nothing is cached yet) surfaces the error to the caller.
func (r *reloadingTLS) reloadLocked() error {
	r.lastCheck = time.Now()

	caData := r.caData
	caPool := r.caPool
	if r.caFile != "" {
		data, err := os.ReadFile(r.caFile)
		if err != nil {
			if r.caPool == nil {
				return err
			}
			return nil
		}
		caData = data
	}

	var cert *tls.Certificate
	if r.certFile != "" || r.keyFile != "" {
		loaded, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
		if err != nil {
			if r.cert == nil {
				return err
			}
			return nil
		}
		cert = &loaded
	}

	caChanged := !bytes.Equal(caData, r.caData)
	if caChanged {
		pool := x509.NewCertPool()
		if len(caData) > 0 && !pool.AppendCertsFromPEM(caData) {
			if r.caPool == nil {
				return errors.Errorf("failed to parse cluster-proxy CA bundle %s", r.caFile)
			}
			caChanged = false
		} else {
			caPool = pool
		}
	}

	certChanged := !certsEqual(r.cert, cert)
	had := r.caPool != nil || r.cert != nil

	r.caData = caData
	r.caPool = caPool
	r.cert = cert

	if had && (caChanged || certChanged) && r.onRotate != nil {
		r.onRotate()
	}
	return nil
}

// certsEqual reports whether two client certificates carry the same leaf chain.
func certsEqual(a, b *tls.Certificate) bool {
	if a == nil || b == nil {
		return a == b
	}
	if len(a.Certificate) != len(b.Certificate) {
		return false
	}
	for i := range a.Certificate {
		if !bytes.Equal(a.Certificate[i], b.Certificate[i]) {
			return false
		}
	}
	return true
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
	lastUsed  time.Time
}

// clusterProxyUpgradeTransportCap caps the upgrade-transport cache so it cannot
// grow without bound. Entries are keyed by cluster name and there is no
// cluster-deletion hook at this layer, so a fleet whose cluster names churn over
// time would otherwise accumulate one transport per name forever. Far more than
// any realistic live cluster count, this only evicts genuinely stale entries.
const clusterProxyUpgradeTransportCap = 1024

var (
	clusterProxyUpgradeMu         sync.Mutex
	clusterProxyUpgradeTransports = map[string]*clusterProxyUpgradeEntry{}
)

// ClusterProxyUpgradeTransport returns a pooled upgrade (SPDY) transport for the
// given cluster, so exec/attach/port-forward requests reuse one http.Transport
// per cluster instead of allocating a fresh one per request. The shared
// cluster-proxy DialHolder dial routes each connection to the right cluster by
// address. The cache is keyed by cluster name; when a cluster's credential
// rotates the stale transport's idle connections are dropped and it is replaced,
// and the cache is bounded to clusterProxyUpgradeTransportCap entries (evicting
// the least-recently-used) so it cannot grow without bound under name churn.
func ClusterProxyUpgradeTransport(clusterName string, transportCfg *k8stransport.Config, dial k8snet.DialFunc) (*http.Transport, error) {
	credKey := clusterProxyUpgradeKey(transportCfg)

	clusterProxyUpgradeMu.Lock()
	defer clusterProxyUpgradeMu.Unlock()
	if entry, ok := clusterProxyUpgradeTransports[clusterName]; ok {
		if entry.credKey == credKey {
			entry.lastUsed = time.Now()
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
	evictStaleUpgradeTransportsLocked(clusterName)
	clusterProxyUpgradeTransports[clusterName] = &clusterProxyUpgradeEntry{credKey: credKey, transport: t, lastUsed: time.Now()}
	return t, nil
}

// evictStaleUpgradeTransportsLocked drops the least-recently-used entries (other
// than keepName, which is about to be (re)inserted) until there is room for one
// more. Callers hold clusterProxyUpgradeMu.
func evictStaleUpgradeTransportsLocked(keepName string) {
	for len(clusterProxyUpgradeTransports) >= clusterProxyUpgradeTransportCap {
		var oldestName string
		var oldest time.Time
		for name, entry := range clusterProxyUpgradeTransports {
			if name == keepName {
				continue
			}
			if oldestName == "" || entry.lastUsed.Before(oldest) {
				oldestName, oldest = name, entry.lastUsed
			}
		}
		if oldestName == "" {
			return
		}
		clusterProxyUpgradeTransports[oldestName].transport.CloseIdleConnections()
		delete(clusterProxyUpgradeTransports, oldestName)
	}
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

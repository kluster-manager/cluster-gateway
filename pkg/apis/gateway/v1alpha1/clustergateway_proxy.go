/*
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
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	gopath "path"
	"regexp"
	"strings"
	"time"

	"k8s.io/apiserver/pkg/server"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	"k8s.io/utils/strings/slices"

	"github.com/kluster-manager/cluster-auth/apis/authentication/v1alpha1"
	"github.com/kluster-manager/cluster-gateway/pkg/config"
	"github.com/kluster-manager/cluster-gateway/pkg/featuregates"
	"github.com/kluster-manager/cluster-gateway/pkg/metrics"
	"github.com/kluster-manager/cluster-gateway/pkg/util/singleton"

	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	apiproxy "k8s.io/apimachinery/pkg/util/proxy"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/endpoints/request"
	registryrest "k8s.io/apiserver/pkg/registry/rest"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	kmapi "kmodules.xyz/client-go/api/v1"
	"sigs.k8s.io/apiserver-runtime/pkg/builder/resource"
	"sigs.k8s.io/apiserver-runtime/pkg/builder/resource/resourcerest"
	contextutil "sigs.k8s.io/apiserver-runtime/pkg/util/context"
	"sigs.k8s.io/apiserver-runtime/pkg/util/loopback"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ resource.SubResource = &ClusterGatewayProxy{}
var _ registryrest.Storage = &ClusterGatewayProxy{}
var _ resourcerest.Connecter = &ClusterGatewayProxy{}

var proxyMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}

var ImpersonatorKey = ".metadata.impersonator"

// ClusterGatewayProxy is a subresource for ClusterGateway which allows user to proxy
// kubernetes resource requests to the managed cluster.
type ClusterGatewayProxy struct {
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ClusterGatewayProxyOptions struct {
	metav1.TypeMeta

	// Path is the target api path of the proxy request.
	// e.g. "/healthz", "/api/v1"
	Path string `json:"path"`

	// Impersonate indicates whether to impersonate as the original
	// user identity from the request context after proxying to the
	// target cluster.
	// Note that this will requires additional RBAC settings inside
	// the target cluster for the impersonated users (i.e. the end-
	// user using the proxy subresource.).
	Impersonate bool `json:"impersonate"`
}

func (c *ClusterGatewayProxy) SubResourceName() string {
	return "proxy"
}

func (c *ClusterGatewayProxy) New() runtime.Object {
	return &ClusterGatewayProxyOptions{}
}

func (in *ClusterGatewayProxy) Destroy() {}

func (c *ClusterGatewayProxy) Connect(ctx context.Context, id string, options runtime.Object, r registryrest.Responder) (http.Handler, error) {
	ts := time.Now()

	proxyOpts, ok := options.(*ClusterGatewayProxyOptions)
	if !ok {
		return nil, fmt.Errorf("invalid options object: %#v", options)
	}

	parentStorage, ok := contextutil.GetParentStorageGetter(ctx)
	if !ok {
		return nil, fmt.Errorf("no parent storage found")
	}
	parentObj, err := parentStorage.Get(ctx, id, &metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("no such cluster %v", id)
	}
	clusterGateway := parentObj.(*ClusterGateway)

	reqInfo, _ := request.RequestInfoFrom(ctx)
	factory := request.RequestInfoFactory{
		APIPrefixes:          sets.NewString("api", "apis"),
		GrouplessAPIPrefixes: sets.NewString("api"),
	}
	proxyReqInfo, _ := factory.NewRequestInfo(&http.Request{
		URL: &url.URL{
			Path: proxyOpts.Path,
		},
		Method: strings.ToUpper(reqInfo.Verb),
	})
	proxyReqInfo.Verb = reqInfo.Verb

	if config.AuthorizateProxySubpath {
		user, _ := request.UserFrom(ctx)
		var attr authorizer.Attributes
		if proxyReqInfo.IsResourceRequest {
			attr = authorizer.AttributesRecord{
				User:        user,
				APIGroup:    proxyReqInfo.APIGroup,
				APIVersion:  proxyReqInfo.APIVersion,
				Resource:    proxyReqInfo.Resource,
				Subresource: proxyReqInfo.Subresource,
				Namespace:   proxyReqInfo.Namespace,
				Name:        proxyReqInfo.Name,
				Verb:        proxyReqInfo.Verb,
			}
		} else {
			path, _ := url.ParseRequestURI(proxyReqInfo.Path)
			attr = authorizer.AttributesRecord{
				User: user,
				Path: path.Path,
				Verb: proxyReqInfo.Verb,
			}
		}

		decision, reason, err := loopback.GetAuthorizer().Authorize(ctx, attr)
		if err != nil {
			return nil, errors.Wrapf(err, "authorization failed due to %s", reason)
		}
		if decision != authorizer.DecisionAllow {
			return nil, fmt.Errorf("proxying by user %v is forbidden authorization failed", user.GetName())
		}
	}

	return &proxyHandler{
		parentName:     id,
		path:           proxyOpts.Path,
		impersonate:    proxyOpts.Impersonate,
		clusterGateway: clusterGateway,
		responder:      r,
		finishFunc: func(code int) {
			metrics.RecordProxiedRequestsByResource(proxyReqInfo.Resource, proxyReqInfo.Verb, code)
			metrics.RecordProxiedRequestsByCluster(id, code)
			metrics.RecordProxiedRequestsDuration(proxyReqInfo.Resource, proxyReqInfo.Verb, id, code, time.Since(ts))
		},
	}, nil
}

func (c *ClusterGatewayProxy) NewConnectOptions() (runtime.Object, bool, string) {
	return &ClusterGatewayProxyOptions{}, true, "path"
}

func (c *ClusterGatewayProxy) ConnectMethods() []string {
	return proxyMethods
}

var _ resource.QueryParameterObject = &ClusterGatewayProxyOptions{}

func (in *ClusterGatewayProxyOptions) ConvertFromUrlValues(values *url.Values) error {
	in.Path = values.Get("path")
	in.Impersonate = values.Get("impersonate") == "true"
	return nil
}

var _ http.Handler = &proxyHandler{}

// +k8s:openapi-gen=false
type proxyHandler struct {
	parentName     string
	path           string
	impersonate    bool
	clusterGateway *ClusterGateway
	responder      registryrest.Responder
	finishFunc     func(code int)
}

var (
	apiPrefix = "/apis/" + config.MetaApiGroupName + "/" + config.MetaApiVersionName + "/clustergateways/"
	apiSuffix = "/proxy"
)

type proxyResponseWriter struct {
	http.ResponseWriter
	http.Hijacker
	http.Flusher
	statusCode int
}

func (in *proxyResponseWriter) WriteHeader(statusCode int) {
	in.statusCode = statusCode
	in.ResponseWriter.WriteHeader(statusCode)
}

func newProxyResponseWriter(_writer http.ResponseWriter) *proxyResponseWriter {
	writer := &proxyResponseWriter{ResponseWriter: _writer, statusCode: http.StatusOK}
	writer.Hijacker, _ = _writer.(http.Hijacker)
	writer.Flusher, _ = _writer.(http.Flusher)
	return writer
}

func (p *proxyHandler) ServeHTTP(_writer http.ResponseWriter, request *http.Request) {
	writer := newProxyResponseWriter(_writer)
	defer func() {
		p.finishFunc(writer.statusCode)
	}()
	cluster := p.clusterGateway
	if cluster.Spec.Access.Credential == nil {
		responsewriters.InternalError(writer, request, fmt.Errorf("proxying cluster %s not support due to lacking credentials", cluster.Name))
		return
	}

	// Go 1.19 removes the URL clone in WithContext method and therefore change
	// to deep copy here
	newReq := request.Clone(request.Context())
	newReq.Header = utilnet.CloneHeader(request.Header)
	newReq.URL.Path = p.path

	urlAddr, err := GetEndpointURL(cluster)
	if err != nil {
		responsewriters.InternalError(writer, request, errors.Wrapf(err, "failed parsing endpoint for cluster %s", cluster.Name))
		return
	}
	host, _, _ := net.SplitHostPort(urlAddr.Host)
	path := strings.TrimPrefix(request.URL.Path, apiPrefix+p.parentName+apiSuffix)
	newReq.Host = host
	newReq.URL.Path = gopath.Join(urlAddr.Path, path)
	newReq.URL.RawQuery = unescapeQueryValues(request.URL.Query()).Encode()
	newReq.RequestURI = newReq.URL.RequestURI()

	cfg, err := NewConfigFromCluster(request.Context(), cluster)
	if err != nil {
		responsewriters.InternalError(writer, request, errors.Wrapf(err, "failed creating cluster proxy client config %s", cluster.Name))
		return
	}
	if p.impersonate || utilfeature.DefaultFeatureGate.Enabled(featuregates.ClientIdentityPenetration) {
		cfg.Impersonate = p.getImpersonationConfig(request)
	}

	rt, err := restclient.TransportFor(cfg)
	if err != nil {
		responsewriters.InternalError(writer, request, errors.Wrapf(err, "failed creating cluster proxy client %s", cluster.Name))
		return
	}
	proxy := apiproxy.NewUpgradeAwareHandler(
		&url.URL{
			Scheme:   urlAddr.Scheme,
			Path:     newReq.URL.Path,
			Host:     urlAddr.Host,
			RawQuery: request.URL.RawQuery,
		},
		rt,
		false,
		false,
		nil)

	const defaultFlushInterval = 200 * time.Millisecond
	transportCfg, err := cfg.TransportConfig()
	if err != nil {
		responsewriters.InternalError(writer, request, errors.Wrapf(err, "failed creating transport config %s", cluster.Name))
		return
	}
	tlsConfig, err := transport.TLSConfigFor(transportCfg)
	if err != nil {
		responsewriters.InternalError(writer, request, errors.Wrapf(err, "failed creating tls config %s", cluster.Name))
		return
	}
	upgrader, err := transport.HTTPWrappersForConfig(transportCfg, apiproxy.MirrorRequest)
	if err != nil {
		responsewriters.InternalError(writer, request, errors.Wrapf(err, "failed creating upgrader client %s", cluster.Name))
		return
	}
	upgrading := utilnet.SetOldTransportDefaults(&http.Transport{
		TLSClientConfig: tlsConfig,
		DialContext:     cfg.Dial,
	})
	proxy.UpgradeTransport = apiproxy.NewUpgradeRequestRoundTripper(
		upgrading,
		RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			newReq := utilnet.CloneRequest(req)
			return upgrader.RoundTrip(newReq)
		}))
	proxy.Transport = rt
	proxy.FlushInterval = defaultFlushInterval
	proxy.Responder = ErrorResponderFunc(func(w http.ResponseWriter, req *http.Request, err error) {
		p.responder.Error(err)
	})
	proxy.ServeHTTP(writer, newReq)
}

type noSuppressPanicError struct{}

func (noSuppressPanicError) Write(p []byte) (n int, err error) {
	// skip "suppressing panic for copyResponse error in test; copy error" error message
	// that ends up in CI tests on each kube-apiserver termination as noise and
	// everybody thinks this is fatal.
	if strings.Contains(string(p), "suppressing panic") {
		return len(p), nil
	}
	return os.Stderr.Write(p)
}

// +k8s:deepcopy-gen=false
type RoundTripperFunc func(req *http.Request) (*http.Response, error)

func (fn RoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

var _ apiproxy.ErrorResponder = ErrorResponderFunc(nil)

// +k8s:deepcopy-gen=false
type ErrorResponderFunc func(w http.ResponseWriter, req *http.Request, err error)

func (e ErrorResponderFunc) Error(w http.ResponseWriter, req *http.Request, err error) {
	e(w, req, err)
}

func (p *proxyHandler) getImpersonationConfig(req *http.Request) restclient.ImpersonationConfig {
	user, _ := request.UserFrom(req.Context())
	if p.clusterGateway.Spec.ProxyConfig != nil {
		matched, ruleName, projected, err := ExchangeIdentity(&p.clusterGateway.Spec.ProxyConfig.Spec.ClientIdentityExchanger, user, p.parentName)
		if err != nil {
			klog.Errorf("exchange identity with cluster config error: %w", err)
		}
		if matched {
			klog.Infof("identity exchanged with rule `%s` in the proxy config from cluster `%s`", ruleName, p.clusterGateway.Name)
			return *projected
		}
	}
	matched, ruleName, projected, err := ExchangeIdentity(&GlobalClusterGatewayProxyConfiguration.Spec.ClientIdentityExchanger, user, p.parentName)
	if err != nil {
		klog.Errorf("exchange identity with global config error: %w", err)
	}
	if matched {
		klog.Infof("identity exchanged with rule `%s` in the proxy config from global config", ruleName)
		return *projected
	}

	isSA := strings.HasPrefix(user.GetName(), "system:serviceaccount:")

	extras := map[string][]string{}
	for k, v := range user.GetExtra() {
		/*
			`kube-apiserver` added:
			  - `alpha` support (guarded by the `ServiceAccountTokenJTI` feature gate) for adding a `jti` (JWT ID) claim to service account tokens it issues, adding an `authentication.kubernetes.io/credential-id` audit annotation in audit logs when the tokens are issued, and `authentication.kubernetes.io/credential-id` entry in the extra user info when the token is used to authenticate.
			  - `alpha` support (guarded by the `ServiceAccountTokenPodNodeInfo` feature gate) for including the node name (and uid, if the node exists) as additional claims in service account tokens it issues which are bound to pods, and `authentication.kubernetes.io/node-name` and `authentication.kubernetes.io/node-uid` extra user info when the token is used to authenticate.
			cluster-gateway s/a is not given permission to impersonate such user extras.
			So, such user extras must be filtered out.
		*/
		if !isSA || !strings.HasPrefix(k, "authentication.kubernetes.io/") {
			extras[k] = v
		}
	}

	if isSA {
		// for trickster
		saParts := strings.Split(user.GetName(), ":")
		if len(saParts) == 4 && saParts[2] == config.ClusterAuthNamespace {
			var accounts v1alpha1.AccountList
			if err := singleton.GetClient().List(context.TODO(), &accounts, client.MatchingFields{ImpersonatorKey: saParts[3]}); err == nil && len(accounts.Items) == 1 {
				ac := accounts.Items[0]
				extras := make(map[string][]string, len(ac.Spec.Extra))
				for k, v := range ac.Spec.Extra {
					extras[k] = v
				}

				groups := []string{
					"system:authenticated",
					"system:serviceaccounts",
				}
				acParts := strings.SplitN(ac.Spec.Username, ":", 4)
				if len(acParts) == 4 {
					groups = append(groups, "system:serviceaccounts:"+acParts[2])
				}

				return restclient.ImpersonationConfig{
					UID:      ac.Spec.UID,
					UserName: ac.Spec.Username,
					Groups:   groups,
					Extra:    extras,
				}
			}
		}
	} else {
		var ac v1alpha1.Account
		if err := singleton.GetClient().Get(context.TODO(), client.ObjectKey{Name: user.GetName()}, &ac); err == nil {
			for k, v := range ac.Spec.Extra {
				extras[k] = v
			}
			var groups []string
			if clientOrgId := extras[kmapi.AceOrgIDKey]; len(clientOrgId) == 1 {
				if v, ok := ac.Spec.Groups[clientOrgId[0]]; ok {
					groups = append(v, fmt.Sprintf("ace.org.%v", clientOrgId[0]))
				} else {
					delete(extras, kmapi.AceOrgIDKey)
				}
			}

			return restclient.ImpersonationConfig{
				UID:      ac.Spec.UID,
				UserName: ac.Spec.Username,
				Groups:   groups,
				Extra:    extras,
			}
		}
	}

	return restclient.ImpersonationConfig{
		UID:      user.GetUID(),
		UserName: user.GetName(),
		Groups:   user.GetGroups(),
		Extra:    extras,
	}
}

// NewClusterGatewayProxyRequestEscaper wrap the base http.Handler and escape
// the dryRun parameter. Otherwise, the dryRun request will be blocked by
// apiserver middlewares
func NewClusterGatewayProxyRequestEscaper(delegate http.Handler) http.Handler {
	return &clusterGatewayProxyRequestEscaper{delegate: delegate}
}

type clusterGatewayProxyRequestEscaper struct {
	delegate http.Handler
}

var (
	clusterGatewayProxyPathPattern = regexp.MustCompile(strings.Join([]string{
		server.APIGroupPrefix,
		config.MetaApiGroupName,
		config.MetaApiVersionName,
		"clustergateways",
		"[a-z0-9]([-a-z0-9]*[a-z0-9])?",
		"proxy"}, "/"))
	clusterGatewayProxyQueryKeysToEscape = []string{"dryRun"}
	clusterGatewayProxyEscaperPrefix     = "__"
)

func (in *clusterGatewayProxyRequestEscaper) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if clusterGatewayProxyPathPattern.MatchString(req.URL.Path) {
		newReq := req.Clone(req.Context())
		q := req.URL.Query()
		for _, k := range clusterGatewayProxyQueryKeysToEscape {
			if q.Has(k) {
				q.Set(clusterGatewayProxyEscaperPrefix+k, q.Get(k))
				q.Del(k)
			}
		}
		newReq.URL.RawQuery = q.Encode()
		req = newReq
	}
	in.delegate.ServeHTTP(w, req)
}

func unescapeQueryValues(values url.Values) url.Values {
	unescaped := url.Values{}
	for k, vs := range values {
		if strings.HasPrefix(k, clusterGatewayProxyEscaperPrefix) &&
			slices.Contains(clusterGatewayProxyQueryKeysToEscape,
				strings.TrimPrefix(k, clusterGatewayProxyEscaperPrefix)) {
			k = strings.TrimPrefix(k, clusterGatewayProxyEscaperPrefix)
		}
		unescaped[k] = vs
	}
	return unescaped
}

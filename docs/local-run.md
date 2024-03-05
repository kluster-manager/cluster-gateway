# Running Non-Etcd Apiserver Locally

### Setting Up Environment

1. Build the container:

```shell
docker build \
  -t "cluster-gateway:v0.0.0-non-etcd" \
  -f cmd/apiserver/Dockerfile .
```

2. Spawn a local KinD cluster:

```shell
kind create cluster --name hub
kind export kubeconfig --kubeconfig /tmp/hub.kubeconfig --name hub
kind load docker-image "cluster-gateway:v0.0.0-non-etcd" --name hub
```

3. Apply the manifests below:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gateway-deployment
  labels:
    app: gateway
spec:
  replicas: 3
  selector:
    matchLabels:
      app: gateway
  template:
    metadata:
      labels:
        app: gateway
    spec:
      containers:
        - name: gateway
          image: "cluster-gateway:v0.0.0-non-etcd"
          command:
            - ./apiserver
            - --secure-port=9443
            - --secret-namespace=default
            - --feature-gates=APIPriorityAndFairness=false
          ports:
            - containerPort: 9443
---
apiVersion: v1
kind: Service
metadata:
  name: gateway-service
spec:
  selector:
    app: gateway
  ports:
    - protocol: TCP
      port: 9443
      targetPort: 9443
---
apiVersion: apiregistration.k8s.io/v1
kind: APIService
metadata:
  name: v1alpha1.gateway.open-cluster-management.io
  labels:
    api: cluster-extension-apiserver
    apiserver: "true"
spec:
  version: v1alpha1
  group: gateway.open-cluster-management.io
  groupPriorityMinimum: 2000
  service:
    name: gateway-service
    namespace: default
    port: 9443
  versionPriority: 10
  insecureSkipTLSVerify: true
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: system::extension-apiserver-authentication-reader:cluster-gateway
  namespace: kube-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: extension-apiserver-authentication-reader
subjects:
  - kind: ServiceAccount
    name: default
    namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: default
  name: cluster-gateway-secret-reader
rules:
  - apiGroups:
      - ""
    resources:
      - "secrets"
    verbs:
      - get
      - list
      - watch
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: cluster-gateway-secret-reader
  namespace: default
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: cluster-gateway-secret-reader
subjects:
  - kind: ServiceAccount
    name: default
    namespace: default
---
```

4. Check if apiserver aggregation working properly:

```shell
$ KUBECONFIG=/tmp/hub.kubeconfig kubectl api-resources | grep clustergateway
$ KUBECONFIG=/tmp/hub.kubeconfig kubectl get clustergateway # A 404 error is expected
```

### Proxying Multi-Cluster

1. Prepare a second cluster `managed1` that accessible from `hub`'s network.

2.1. Creates a secret containing X509 certificate/key to the hub cluster:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: managed1
  labels:
    gateway.open-cluster-management.io/cluster-credential-type: X509
type: Opaque # <--- Has to be opaque
data:
  endpoint: "..." # Should NOT be 127.0.0.1
  ca.crt: "..." # ca cert for cluster "managed1"
  tls.crt: "..." # x509 cert for cluster "managed1"
  tls.key: "..." # private key for cluster "managed1"
```

2.2. (Alternatively) Create a secret containing service-account token to the hub cluster:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: managed1
  labels:
    gateway.open-cluster-management.io/cluster-credential-type: ServiceAccountToken
type: Opaque # <--- Has to be opaque
data:
  endpoint: "..." # ditto
  ca.crt: "..." # ditto
  token: "..." # working jwt token
```

3. Proxy to cluster `managed1`'s `/healthz` endpoint

```shell
$ KUBECONFIG=/tmp/hub.kubeconfig kubectl get \
      --raw="/apis/gateway.open-cluster-management.io/v1alpha1/clustergateways/managed1/proxy/healthz"
```

4. Craft a dedicated kubeconfig for proxying `managed1` from `hub` cluster:

```shell
$ cat /tmp/hub.kubeconfig \
    | sed 's/\(server: .*\)/\1\/apis\/gateway.open-cluster-management.io\/v1alpha1\/clustergateways\/managed1\/proxy\//' \
    > /tmp/hub-managed1.kubeconfig
```

try the tweaked kubeconfig:

```shell
# list namespaces under cluster managed1
KUBECONFIG=/tmp/hub-managed1.kubeconfig kubectl get ns
```

### Clean up

1. Deletes the sandbox clusters:

```shell
$ kind delete cluster --name tmp
```
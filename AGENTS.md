# AGENTS.md

This file provides guidance to coding agents (e.g. Claude Code, claude.ai/code) when working with code in this repository.

## Repository purpose

Go module `github.com/kluster-manager/cluster-gateway` — an aggregated Kubernetes APIserver that proxies API traffic to multiple managed Kubernetes clusters. Registers `gateway.open-cluster-management.io/ClusterGateway` (a virtual, read-only resource that wraps namespaced kubeconfig `Secret`s) on the hub. Each `ClusterGateway` exposes a `proxy` subresource modeled on the native `pod/proxy` / `service/proxy` — so you can do `kubectl get --raw=/apis/gateway.open-cluster-management.io/v1alpha1/clustergateways/<cluster>/proxy/api/v1/pods` to reach a managed cluster's API.

Key design properties (from `README.md`):
- **Etcd-free**: `ClusterGateway` resources are projected from `Secret`s in a namespace of the hosting cluster; no dedicated etcd is required.
- **Scalable**: the gateway scales out horizontally.

Produces two binaries: `apiserver` and `addon-manager`. The repo is an OCM addon — the addon-manager registers cluster-gateway as a `ManagedClusterAddOn` on the OCM hub.

The local filesystem path is `open-cluster-management.io/cluster-gateway`; the **actual Go module is `github.com/kluster-manager/cluster-gateway`** and is mirrored from the upstream `oam-dev/cluster-gateway`. This repo tracks upstream with AppsCode patches.

## Architecture

- `cmd/apiserver/` — the aggregated API server binary. `main.go` + `Dockerfile`.
- `cmd/addon-manager/` — the OCM addon manager binary (registers the gateway as a `ClusterManagementAddOn`). `main.go` + `Dockerfile`.
- `pkg/apis/`:
  - `gateway/v1alpha1/` — `ClusterGateway` and `ClusterGatewayProxyConfig` types. `transport/` contains the proxy transport logic (TLS, auth flavors). `doc.go` registers the group.
  - `config/v1alpha1/` — gateway server config types (endpoint type, credential type, etc.).
- `pkg/addon/`:
  - `agent/addon.go` — defines the `ManagedClusterAddOn` shape.
  - `controllers/` — `installer.go` (drives `ManifestWork` install), `health.go` (probe).
- `pkg/common/`, `pkg/config/`, `pkg/event/`, `pkg/featuregates/`, `pkg/metrics/`, `pkg/util/` — supporting packages.
- `pkg/generated/` — generated clientset/listers/informers (`client-gen`). Do not hand-edit.
- `charts/` — Helm charts:
  - `cluster-gateway/` — install the aggregated apiserver itself.
  - `cluster-gateway-manager/` — install the addon manager.
- `e2e/` — end-to-end tests against a real OCM hub:
  - `e2e_test.go`, `e2e.go`, `framework/`, `env/` — harness.
  - `kubernetes/`, `ocm/`, `roundtrip/`, `benchmark/` — suite buckets.
- `examples/`, `docs/` — manifests and architecture notes.
- `hack/` — codegen scripts; `Makefile` is a hand-rolled Kubebuilder/controller-runtime style harness (no AppsCode Docker wrapper). Tools (`controller-gen`, `kustomize`, `client-gen`) install into `bin/` on first use.

CRDs live alongside the API types and are rendered via `controller-gen` into the chart `crds/` directories.

## Common commands

This repo uses a **local Go toolchain**, not the AppsCode Docker harness — make sure your Go matches what `go.mod` declares.

- `make manager` (alias `make all`) — `generate fmt vet`, then build the manager binary.
- `make generate` — controller-gen DeepCopy generation.
- `make manifests` — controller-gen CRDs/RBAC.
- `make client-gen` — regenerate client code under `pkg/generated/`.
- `make fmt` — `go fmt ./...`.
- `make vet` — `go vet ./...`.
- `make test` — `generate fmt vet manifests` then Go tests.
- `make run` — run the controller against `~/.kube/config` locally.
- `make local-run` — run against the local kube context with extra dev defaults.
- `make install` / `make uninstall` — kustomize apply/remove CRDs.
- `make deploy` — kustomize-deploy the full stack.
- `make gateway` — build the `cluster-gateway` image.
- `make ocm-addon-manager` — build the addon-manager image.
- `make image` — build both images (`gateway` + `ocm-addon-manager`).
- `make docker-build` — `test`, then docker build.
- `make docker-push` — push the built image.
- `make controller-gen` / `make kustomize` — install the tools into `bin/`.

Run a single Go test:

```
go test ./pkg/apis/gateway/... -run TestName -v
```

End-to-end suite (requires a kind/OCM cluster set up per `e2e/env/`):

```
go test ./e2e/... -v
```

## Conventions

- Module path is `github.com/kluster-manager/cluster-gateway`. Filesystem location under `open-cluster-management.io/` is for layout only — imports use the GitHub path.
- **Upstream is `oam-dev/cluster-gateway`** (the OAM/Crossplane community fork). Prefer rebasing onto upstream over diverging; isolate AppsCode-only patches so they replay cleanly.
- License: Apache-2.0 (`LICENSE`).
- Sign off commits (`git commit -s`).
- API group is `gateway.open-cluster-management.io` — keep that exact group string when adding new resources.
- Do not hand-edit anything under `pkg/generated/` or the controller-gen-generated CRD YAMLs in `charts/*/crds/` — change `pkg/apis/<group>/<version>/*_types.go` and re-run `make generate manifests client-gen`.
- The `ClusterGateway` resource is intentionally **virtual** — it does not have its own etcd storage and is projected from `Secret`s in the hosting cluster. Do not "fix" this by adding a real storage backend; the etcd-free property is one of the project's headline features.
- Two charts, two binaries — keep `charts/cluster-gateway` (apiserver) and `charts/cluster-gateway-manager` (addon-manager) in sync with their respective `cmd/` entries.

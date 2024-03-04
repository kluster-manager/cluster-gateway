
# Image URL to use all building/pushing image targets
IMG ?= controller:latest
IMAGE_REGISTRY_NAME ?= ghcr.io/kluster-manager
IMG_TAG ?= latest
CRD_OPTIONS ?= "crd:crdVersions={v1},allowDangerousTypes=true,generateEmbeddedObjectMeta=true"

OS?=linux
ARCH?=amd64

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

VERSION=v0.0.21

all: manager

# Run tests
test: generate fmt vet manifests
	go test ./pkg/... -coverprofile cover.out

# Build manager binary
manager: generate fmt vet
	go build -o bin/manager ./cmd/apiserver/main.go

# Run against the configured Kubernetes cluster in ~/.kube/config
run: generate fmt vet manifests
	go run ./cmd/apiserver/main.go

local-run:
	go run ./cmd/apiserver/main.go \
	--standalone-debug-mode=true \
    --bind-address=127.0.0.1 \
    --etcd-servers=127.0.0.1:2379 \
    --secure-port=9443

# Install CRDs into a cluster
install: manifests kustomize
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

# Uninstall CRDs from a cluster
uninstall: manifests kustomize
	$(KUSTOMIZE) build config/crd | kubectl delete -f -

# Deploy controller in the configured Kubernetes cluster in ~/.kube/config
deploy: manifests kustomize
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | kubectl apply -f -

# Run go fmt against code
fmt:
	go fmt ./...

# Run go vet against code
vet:
	go vet ./...

# Build the docker image
docker-build: test
	docker build . -t ${IMG}

# Push the docker image
docker-push:
	docker push ${IMG}

# find or download controller-gen
# download controller-gen if necessary
controller-gen:
ifeq (, $(shell which controller-gen))
	@{ \
	set -e ;\
	CONTROLLER_GEN_TMP_DIR=$$(mktemp -d) ;\
	cd $$CONTROLLER_GEN_TMP_DIR ;\
	go mod init tmp ;\
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.14.0 ;\
	rm -rf $$CONTROLLER_GEN_TMP_DIR ;\
	}
CONTROLLER_GEN=$(GOBIN)/controller-gen
else
CONTROLLER_GEN=$(shell which controller-gen)
endif

kustomize:
ifeq (, $(shell which kustomize))
	@{ \
	set -e ;\
	KUSTOMIZE_GEN_TMP_DIR=$$(mktemp -d) ;\
	cd $$KUSTOMIZE_GEN_TMP_DIR ;\
	go mod init tmp ;\
	go get sigs.k8s.io/kustomize/kustomize/v3@v3.5.4 ;\
	rm -rf $$KUSTOMIZE_GEN_TMP_DIR ;\
	}
KUSTOMIZE=$(GOBIN)/kustomize
else
KUSTOMIZE=$(shell which kustomize)
endif

client-gen:
	go install sigs.k8s.io/apiserver-runtime/tools/apiserver-runtime-gen@v1.1.1
	apiserver-runtime-gen \
	--module github.com/kluster-manager/cluster-gateway \
	-g deepcopy-gen,client-gen,openapi-gen \
	--versions=github.com/kluster-manager/cluster-gateway/pkg/apis/gateway/v1alpha1 \
	--install-generators=false
	rm -rf pkg/generated/clientset/versioned/typed/gateway/v1alpha1/fake
	rm -rf pkg/generated/clientset/versioned/fake

generate: controller-gen
	${CONTROLLER_GEN} object:headerFile="hack/boilerplate.go.txt" paths="./pkg/apis/config/..."

manifests: controller-gen
	${CONTROLLER_GEN} $(CRD_OPTIONS) \
		paths="./pkg/apis/config/..." \
		rbac:roleName=manager-role \
		output:crd:artifacts:config=hack/crd/bases

gateway:
	docker build -t $(IMAGE_REGISTRY_NAME)/cluster-gateway:${IMG_TAG} \
		--build-arg OS=${OS} \
		--build-arg ARCH=${ARCH} \
		-f cmd/apiserver/Dockerfile \
		.

ocm-addon-manager:
	docker build -t $(IMAGE_REGISTRY_NAME)/cluster-gateway-manager:${IMG_TAG} \
		--build-arg OS=${OS} \
		--build-arg ARCH=${ARCH} \
		-f cmd/addon-manager/Dockerfile \
		.

image: gateway ocm-addon-manager

e2e-binary:
	mkdir -p bin
	go test -o bin/e2e -c ./e2e/

e2e-binary-ocm:
	mkdir -p bin
	go test -o bin/e2e.ocm -c ./e2e/ocm/

e2e-bench-binary:
	go test -c ./e2e/benchmark/

test-e2e: e2e-binary
	./bin/e2e --test-cluster=loopback

test-e2e-ocm: e2e-binary-ocm
	./bin/e2e.ocm --test-cluster=loopback

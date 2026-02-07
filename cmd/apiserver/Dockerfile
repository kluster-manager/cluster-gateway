# Build the manager binary
FROM golang:1.25 as builder

WORKDIR /workspace

COPY . .

ARG API_GROUP_NAME=gateway.open-cluster-management.io

# Build
RUN CGO_ENABLED=0 \
    go build \
        -ldflags="-X 'github.com/kluster-manager/cluster-gateway/pkg/config.MetaApiGroupName=${API_GROUP_NAME}'" \
        -o apiserver \
        cmd/apiserver/main.go

RUN CGO_ENABLED=0 \
    go build \
        -o patch \
        hack/patch/main.go

FROM registry.access.redhat.com/ubi9/ubi-minimal:latest

WORKDIR /
COPY --from=builder /workspace/apiserver /
COPY --from=builder /workspace/patch /

USER 65534

ENTRYPOINT ["/apiserver"]

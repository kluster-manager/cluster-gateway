# Build the manager binary
FROM golang:1.24 as builder

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

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/base-debian12

WORKDIR /
COPY --from=builder /workspace/apiserver /
COPY --from=builder /workspace/patch /

USER 65534

ENTRYPOINT ["/apiserver"]

ARG OS=linux
ARG ARCH=amd64
# Build the manager binary
FROM golang:1.25 as builder

WORKDIR /workspace

COPY . .

# Build
RUN CGO_ENABLED=0 \
    go build \
        -a -o addon-manager \
        cmd/addon-manager/main.go

ARG ARCH
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest

WORKDIR /
COPY --from=builder /workspace/addon-manager /

USER 65534

ENTRYPOINT ["/addon-manager"]

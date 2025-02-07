ARG OS=linux
ARG ARCH=amd64
# Build the manager binary
FROM golang:1.24 as builder

WORKDIR /workspace

COPY . .

# Build
RUN CGO_ENABLED=0 \
    go build \
        -a -o addon-manager \
        cmd/addon-manager/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
ARG ARCH
FROM gcr.io/distroless/base-debian12

WORKDIR /
COPY --from=builder /workspace/addon-manager /

USER 65534

ENTRYPOINT ["/addon-manager"]

# Builds the official Tailscale v1.102.3 source at its verified commit with the
# narrow PortlistServices eventbus fix. Upstream issue: #18297.

FROM golang:1.26-alpine AS build
ARG TAILSCALE_COMMIT=53a0d659afa51835dd7a9283873cca44261454f8
ARG VERSION_LONG=1.102.3
ARG VERSION_SHORT=1.102.3
ARG VERSION_GIT_HASH=53a0d659afa51835dd7a9283873cca44261454f8
WORKDIR /go/src/tailscale
RUN apk add --no-cache git ca-certificates
RUN git init . \
 && git remote add origin https://github.com/tailscale/tailscale.git \
 && git fetch --depth=1 origin ${TAILSCALE_COMMIT} \
 && git checkout --detach ${TAILSCALE_COMMIT}
COPY tailscale-portlist.patch /tmp/tailscale-portlist.patch
RUN git apply /tmp/tailscale-portlist.patch
RUN go mod download
RUN go install \
    github.com/aws/aws-sdk-go-v2/aws \
    github.com/aws/aws-sdk-go-v2/config \
    gvisor.dev/gvisor/pkg/tcpip/adapters/gonet \
    gvisor.dev/gvisor/pkg/tcpip/stack \
    golang.org/x/crypto/ssh \
    golang.org/x/crypto/acme \
    github.com/coder/websocket \
    github.com/mdlayher/netlink
ARG TARGETARCH
RUN GOARCH=${TARGETARCH} go install \
    -tags=ts_kube,ts_package_container \
    -ldflags="-X tailscale.com/version.longStamp=${VERSION_LONG} -X tailscale.com/version.shortStamp=${VERSION_SHORT} -X tailscale.com/version.gitCommitStamp=${VERSION_GIT_HASH}" \
    ./cmd/tailscale ./cmd/tailscaled ./cmd/containerboot

# Keep the official container filesystem, entry command, and runtime support.
FROM tailscale/tailscale@sha256:8c42c4574ab066384fcb72f69e086a2ff1dd3652eb6f56856cee34bcf0d2f680
COPY --from=build /go/bin/* /usr/local/bin/

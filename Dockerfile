# syntax=docker/dockerfile:1

FROM golang:1.27.1-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG TEST_PACKAGES=./...
FROM build AS test
RUN go test -mod=readonly -race ${TEST_PACKAGES}

ARG BENCH_PACKAGES=./internal/storage ./internal/relay
ARG BENCHTIME=5s
FROM build AS bench
ARG BENCH_PACKAGES
ARG BENCHTIME
RUN go test -mod=readonly -run '^$' -bench . -benchmem -benchtime=${BENCHTIME} ${BENCH_PACKAGES}

FROM build AS binary
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w" -o /out/tiny ./cmd/tiny

FROM debian:bookworm-slim AS runtime
RUN apt-get update \
    && apt-get install --no-install-recommends -y ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --create-home --home-dir /home/relay --uid 10001 relay \
    && mkdir -p /data \
    && chown relay:relay /data
COPY --from=binary /out/tiny /usr/local/bin/tiny
COPY LICENSE THIRD_PARTY_NOTICES internal/webui/signer.js.license internal/webui/nostr-name.js.license /usr/share/doc/tiny/
USER relay
WORKDIR /home/relay
VOLUME ["/data"]
ENV TINY_DATA_DIR=/data
ENTRYPOINT ["/usr/local/bin/tiny"]
CMD ["serve"]

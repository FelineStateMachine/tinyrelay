# tiny

`tiny` is a self-hosted, multitenant Nostr relay. Each tenant has durable local storage, an owner, relay policy, inbox and outbox delivery, hosted sites, file storage, and optional repository browsing.

## Run locally

Build and start the daemon:

```sh
go build -o tiny ./cmd/tiny
./tiny serve --data-dir ./data --listen :7447
```

For a public deployment, set the relay URL and provision the owner key:

```sh
./tiny serve --data-dir /var/lib/tiny --listen 127.0.0.1:7447 \
  --public-url https://relay.example \
  --provision-owner <64-lowercase-hex-pubkey>
```

The command is `tiny`. Useful commands include:

```text
tiny serve [--data-dir PATH] [--listen :7447]
tiny tenant create --name NAME --owner PUBKEY [--template default] [--source wss://...]
tiny tenant list|enable|disable|host [options]
tiny templates
tiny version
```

Create a permanent tenant with a template:

```sh
tiny tenant create --data-dir /var/lib/tiny \
  --name community --owner <64-lowercase-hex-pubkey> \
  --template chat
```

Run `tiny templates` to see available templates. Templates that read from an upstream relay accept `--source wss://relay.example`.

## Configure and operate

The web interface uses plain HTML pages and forms. Connect a Nostr signer with NIP-07 or Nostr Connect to sign in and authorize management actions. Use the management pages to configure policy, members, relay connections, inbox and outbox jobs, backups, hosted sites, and stored files.

The browsing pages provide repository lists, source files, branches, history, diffs, file previews, raw downloads, synchronization status, and storage status. Access follows the tenant's current permissions.

The `/tools` page reports whether the browser supports native WebMCP controls. See [WebMCP tools](docs/webmcp.md) for browser-agent integration and authorization behavior.

## Deployment

Build the container image:

```sh
docker build --target runtime -t tiny:local .
docker run --rm -p 7447:7447 -v tiny-data:/data \
  tiny:local serve --listen :7447 --public-url https://relay.example
```

For systemd, install [`deploy/tiny.service`](deploy/tiny.service), create `/etc/tiny/tiny.env`, then run:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now tiny
```

Keep the data directory on local durable storage. The process exposes `/healthz` and `/readyz`. An optional diagnostics listener provides `/metrics` and protected pprof endpoints. Set `--otlp-endpoint` or `OTEL_EXPORTER_OTLP_ENDPOINT` when exporting traces.

See [Personal relay deployment](docs/personal-relay.md) for the current Slate installation.

## Test and diagnose

```sh
make test
make test-race
make docker-test
make benchmark
```

Run the Linux race suite on Slate with `SLATE_HOST=slate ./scripts/test-slate.sh`. Run the conformance harness against a started daemon with `node scripts/conformance/run.mjs`; results are written under `artifacts/conformance/`.

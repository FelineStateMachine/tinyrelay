#!/bin/sh
# Join the tailnet in userspace mode, expose an HTTP CONNECT proxy for Caddy,
# then run Caddy. tailscaled dials upstreams by address through SOCKS5 so the
# Host header passes through untouched. TS_AUTHKEY comes from a Fly secret and is only needed the
# first time; afterwards the identity persists on the state volume.
set -eu
STATE="${TS_STATE_DIR:-/var/lib/tailscale}"
mkdir -p /var/run/tailscale "$STATE"
tailscaled --state="$STATE/tailscaled.state" --socket=/var/run/tailscale/tailscaled.sock \
  --tun=userspace-networking --socks5-server=127.0.0.1:1055 &
until [ -S /var/run/tailscale/tailscaled.sock ]; do sleep 1; done
if [ -n "${TS_AUTHKEY:-}" ]; then
  tailscale --socket=/var/run/tailscale/tailscaled.sock up --authkey="$TS_AUTHKEY" --hostname="${TS_HOSTNAME:-tiny-edge}" --accept-dns=false
elif [ -f "$STATE/tailscaled.state" ]; then
  tailscale --socket=/var/run/tailscale/tailscaled.sock up --hostname="${TS_HOSTNAME:-tiny-edge}" --accept-dns=false
else
  echo "TS_AUTHKEY is not set and no tailnet state exists; set the secret and redeploy" >&2
fi
exec caddy run --config /etc/caddy/Caddyfile --adapter caddyfile

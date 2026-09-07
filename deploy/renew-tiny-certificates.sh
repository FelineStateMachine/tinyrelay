#!/usr/bin/env bash
set -euo pipefail
umask 077

test "$(findmnt -n -o UUID --target /srv/tiny)" = 98f49cba-bd8c-432d-a91a-41db66920e90
install -d -m 700 /srv/tiny/certs /srv/tiny/tailscale/certs
docker exec tiny-tailscale tailscale cert --min-validity=336h \
  --cert-file=/var/lib/tailscale/certs/tailnet.crt \
  --key-file=/var/lib/tailscale/certs/tailnet.key tiny.tailbe516a.ts.net
install -m 644 /srv/tiny/tailscale/certs/tailnet.crt /srv/tiny/certs/tailnet.crt
install -m 600 /srv/tiny/tailscale/certs/tailnet.key /srv/tiny/certs/tailnet.key

if ! openssl x509 -checkend 1209600 -noout -in /srv/tiny/certs/tiny.crt >/dev/null 2>&1; then
  openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout /srv/tiny/certs/tiny.key -out /srv/tiny/certs/tiny.csr \
    -subj /CN=tiny -addext subjectAltName=DNS:tiny
  openssl x509 -req -in /srv/tiny/certs/tiny.csr \
    -CA /var/lib/sandboxd/certs/ca.crt -CAkey /var/lib/sandboxd/certs/ca.key \
    -set_serial "0x$(openssl rand -hex 16)" -days 90 -sha256 \
    -copy_extensions copy -out /srv/tiny/certs/tiny.crt
  chmod 644 /srv/tiny/certs/tiny.crt
fi

if [ "$(docker inspect -f '{{.State.Running}}' tiny-https 2>/dev/null || true)" = true ]; then
  docker exec tiny-https caddy reload --config /etc/caddy/Caddyfile --force
fi

# Edge proxy for a tiny relay: Fly terminates TLS, this machine joins the
# tailnet with userspace networking and forwards plain HTTP to the relay host.
FROM caddy:2-alpine
RUN apk add --no-cache tailscale ca-certificates
COPY Caddyfile /etc/caddy/Caddyfile
COPY start.sh /start.sh
RUN chmod +x /start.sh
ENTRYPOINT ["/start.sh"]

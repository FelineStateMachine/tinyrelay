# Personal relay deployment

The personal relay runs on Slate and is available through Tailscale. Open [https://tiny](https://tiny) on this Mac, or use [https://tiny.tailbe516a.ts.net](https://tiny.tailbe516a.ts.net) on your phone with Tailscale connected.

## Storage

The relay data lives on a dedicated 2 TB WD drive mounted at `/srv/tiny`. The drive uses UUID `98f49cba-bd8c-432d-a91a-41db66920e90` and is formatted as ext4.

Persisted directories include:

- `/srv/tiny/data` for relay and tenant data
- `/srv/tiny/tailscale` for Tailscale state
- `/srv/tiny/certs` for certificates
- `/srv/tiny/logs` for service logs

The service checks the drive's UUID before starting and stops if the mount disappears.

## Service operations

The service is `tiny.service`, with certificate renewal scheduled by `tiny-certificates.timer`.

In the Tailscale admin console, open **Machines > tiny > Endpoints** to find the HTTP and HTTPS listeners. The node's key expiry is disabled.

```sh
ssh slate sudo systemctl status tiny
ssh slate sudo systemctl restart tiny
ssh slate sudo systemctl status tiny-certificates.timer
ssh slate docker logs tiny
```

To deploy an update, build the images from a checkout on Slate, then restart the service:

```sh
docker build --target runtime -t tiny:dogfood .
docker build -f deploy/Tailscale.Dockerfile -t tiny-tailscale:dogfood deploy
sudo systemctl restart tiny
```

The owner is `npub1cq2s86qrtadf36eqq3epm38wmhdynrrcwxf84eldk3f94dkv7ttsrulrz5`. Sign in with that identity to manage the relay.

## Web operations

Sign in at the relay URL with a Nostr signer before using owner controls. The Manage pages provide relay configuration, jobs, connections, backups, and file operations. Use Manage data or the browser tools page to start and inspect backups. The [WebMCP guide](webmcp.md) describes native browser-agent controls and their existing signer authorization path.

Download backups from **Manage > Data** and keep a copy on another device.

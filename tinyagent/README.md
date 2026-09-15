# tinyagent

`tinyagent` is the native Tinyrelay connector helper for Hermes Agent. It is the connector name used by Hermes plugin discovery. The Hermes platform registered by the plugin is named `tiny`.

Build the helper from the repository root:

```sh
mkdir -p bin
go build -o bin/tinyagent ./tinyagent
```

Install the plugin in a standard Hermes home and put the helper on `PATH`:

```sh
export HERMES_HOME="${HERMES_HOME:-$HOME/.hermes}"
mkdir -p "$HERMES_HOME/plugins"
cp -R tinyagent/hermes "$HERMES_HOME/plugins/tinyagent"
export PATH="$PWD/bin:$PATH"
```

Configure the connector with the relay URL, the private key used by this Hermes instance, the room to monitor, and the public keys allowed to control it:

```sh
export TINY_RELAY_URL=https://relay.example.test
export TINY_PRIVATE_KEY=YOUR_HEX_PRIVATE_KEY
export TINY_CHANNELS=room-id
export TINY_HOME_CHANNEL=room-id
export TINY_ALLOWED_USERS=OPERATOR_PUBLIC_KEY
export TINY_CLI_PATH="$PWD/bin/tinyagent"
hermes plugins enable tinyagent
hermes gateway run
```

By default, Hermes responds when an allowed user mentions the agent or replies to one of its messages. Set `TINY_REQUIRE_MENTION=false` to respond to all messages from allowed users in the selected rooms. This controls when Hermes responds; notifications still require an explicit mention by the agent.

`TINY_PRIVATE_KEY` is read from the environment by the helper and is never a command-line argument. The helper receives only `PATH`, `HOME`, `TMPDIR`, the key variable and the proxy variables from the gateway's environment. Give that key membership and an agent grant for the selected rooms on Tiny, including attachment access. Use a relay with the [native interaction extension](../docs/extensions/tinyagent.md) for interactive cards. Hermes loads the adapter through its normal plugin system. Attachments use authenticated Tinyrelay endpoints, with size and hash checks.

One gateway runs one Tiny identity at a time. On connect, the adapter takes an exclusive lock for the relay and key under `$HERMES_HOME/state/tinyagent/` and releases it on disconnect. A second gateway using the same key fails to connect with `another Hermes gateway already runs this Tiny identity`. The same directory keeps the delivery cursor, so a restart resumes where the previous run stopped instead of replaying old messages.

If the helper process exits, Hermes reports the Tiny platform as disconnected and the adapter restarts the helper on its own, waiting 1 second before the first attempt and doubling the wait up to 30 seconds between failures. Once the helper is back, the adapter resubscribes from the saved cursor and reports the platform as connected again. Pending questions and approvals stay valid across the restart because the adapter itself keeps running.

The helper exposes a JSON-lines RPC mode for the adapter. Its public commands are:

```sh
tinyagent keygen
TINY_PRIVATE_KEY=YOUR_HEX_PRIVATE_KEY tinyagent rpc --relay https://relay.example.test
tinyagent call --relay https://relay.example.test identity
```

## Relay tools in Hermes

`tinyagent mcp` gives Hermes the relay's own MCP tools. It is a stdio MCP server that Hermes starts like any other `mcp_servers` entry, and it forwards each call to `<relay>/mcp` with a fresh NIP-98 signature from the agent key. Add it to `config.yaml`:

```yaml
mcp_servers:
  tiny:
    command: tinyagent
    args: ["mcp", "--relay", "https://relay.example.test", "--allow-writes"]
    env:
      TINY_PRIVATE_KEY: "${TINY_PRIVATE_KEY}"
```

Hermes resolves `${TINY_PRIVATE_KEY}` from its own environment, including `~/.hermes/.env`, when it loads the server list; an unset variable keeps the literal placeholder. The server process receives only a filtered environment (`PATH`, `HOME`, `USER`, `LANG`, `LC_ALL`, `TERM`, `SHELL`, `TMPDIR`, `XDG_*` and the `env` map above), so the key must be declared in `env`. Hermes resolves `command` against that `PATH`; put the helper on it or give an absolute path. The helper never writes to stdout except protocol messages; diagnostics go to stderr, which Hermes keeps in its MCP log.

Without `--allow-writes` only read tools are offered: repositories, issues, pull requests, files, attachments, rooms, threads, wiki pages, long tasks and status. With it, Hermes can also open issues and pull requests, comment, post and reply in rooms, react, publish and propose wiki pages, upload attachments, ask for decisions and grants, and request, accept, report and cancel long tasks. `--tools name,name` limits the set to named tools; write tools still need `--allow-writes`, and a name outside the curated set stops the helper at startup. `publish_event` and management tools are never offered. Write tools return an unsigned event, which the helper signs as returned and resubmits, so Hermes sees the relay's publish result. Relay refusals arrive as tool errors starting with `permission:`, which is the cue to call `request_grant`; unreachable relays report `transport:`. See [Tinyagent MCP facade](../docs/extensions/tinyagent-mcp.md).

The connector currently targets member access to rooms. Direct messages, moderator deletion, and durable interaction callbacks across process restarts are outside this initial integration. Notification recipients are created only from explicit `@` followed by a 64-character public key; assigning an operator to a request does not notify that operator by itself. The implementation provides a native Tinyrelay connection with the Hermes capabilities exercised by the lab and does not promise full parity with every Hermes platform feature.

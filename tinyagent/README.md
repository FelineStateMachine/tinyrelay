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

`TINY_PRIVATE_KEY` is read from the environment by the helper and is never a command-line argument. Give that key membership and an agent grant for the selected rooms on Tiny, including attachment access. Use a relay with the [native interaction extension](../docs/extensions/tinyagent.md) for interactive cards. Hermes loads the adapter through its normal plugin system. Attachments use authenticated Tinyrelay endpoints, with size and hash checks.

The helper exposes a JSON-lines RPC mode for the adapter. Its public commands are:

```sh
tinyagent keygen
TINY_PRIVATE_KEY=YOUR_HEX_PRIVATE_KEY tinyagent rpc --relay https://relay.example.test
tinyagent call --relay https://relay.example.test identity
```

The connector currently targets member access to rooms. Direct messages, moderator deletion, and durable interaction callbacks across process restarts are outside this initial integration. Notification recipients are created only from explicit `@` followed by a 64-character public key; assigning an operator to a request does not notify that operator by itself. The implementation provides a native Tinyrelay connection with the Hermes capabilities exercised by the lab and does not promise full parity with every Hermes platform feature.

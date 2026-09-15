# Tinyagent MCP facade

| Field | Value |
| --- | --- |
| Identifier | `tinyagent-mcp` |
| Status | Experimental |
| Contract version | 1; no negotiated wire version |
| Owner | `tinyagent` command and `tinyagent/client` transport; `internal/mcp` and `internal/daemon` serve the relay side. |
| Wire | Standard MCP stdio (newline-delimited JSON-RPC 2.0) toward the agent host; the relay's stateless Streamable HTTP endpoint with NIP-98 proofs toward the relay. |

The [catalog's audit baseline and transport rules](README.md) apply. The relay endpoint, its headers and its tool table are specified in [MCP](../mcp.md); this document describes the process that bridges an MCP stdio client to that endpoint. It adds no relay-side wire fields.

## Purpose

Hermes Agent and other MCP hosts speak MCP over stdio with an `initialize` handshake and a long-lived session. The relay endpoint has no handshake and no session: every POST is signed and self-describing. `tinyagent mcp` sits between the two. It runs as a child process of the host, answers the handshake locally and forwards tool calls to the relay with a fresh signature per request. The agent key never leaves the process; the host never sees it.

```sh
tinyagent mcp --relay URL [--key-env TINY_PRIVATE_KEY] [--tools name,name] [--allow-writes]
```

`--relay` names the relay origin, with a tenant prefix such as `https://relay.example.test/r/acme` when the tenant is served under a path. `--key-env` names the environment variable holding the agent's private key as hex; it is never a command-line argument.

## Host protocol

The facade reads one JSON-RPC 2.0 message per line on stdin and writes one per line on stdout. Nothing else is written to stdout; diagnostics go to stderr.

| Method | Behavior |
| --- | --- |
| `initialize` | Answers with the client's `protocolVersion` when it is `2024-11-05`, `2025-03-26`, `2025-06-18` or `2025-11-25`, and with `2025-06-18` otherwise. Capabilities are `{"tools": {}}`; `serverInfo` names `tinyagent`. |
| `notifications/initialized`, `notifications/cancelled` and any other notification | Ignored. A cancelled call still runs to completion on the relay and its answer is discarded by the host. |
| `ping` | Answers `{}`. |
| `tools/list` | The relay's tool table restricted to the exposed set, with the relay's descriptions, input schemas and annotations, followed by `tiny_diagnose`. When the relay's table cannot be loaded, for any reason, the answer is `tiny_diagnose` alone, the failure is written to stderr and the table is fetched again on the next `tools/list` or on the next `tools/call` the relay answers. |
| `tools/call` | Forwarded to the relay as described below. |
| Any other request | JSON-RPC error `-32601`. A message that is not one JSON object answers `-32700`. |

Requests are handled concurrently so `ping` is answered while a tool call is in flight; calls to the relay are serialized and each is bounded to two minutes.

## Relay protocol

Every forwarded call is one POST to `<relay>/mcp` carrying `Authorization: Nostr <base64 kind 27235 event>` bound to `POST`, the full URL and the SHA-256 of the body, the `MCP-Protocol-Version`, `Mcp-Method` and, for `tools/call`, `Mcp-Name` headers, `Accept` and `Content-Type: application/json`, and `params._meta` with the protocol version, empty client capabilities and `tinyagent` as client info. The tool table is fetched with `tools/list` once per process and again after the relay reports an unknown tool or after a fetch that failed.

## Exposed tools

The facade offers a curated subset of the relay's table. A tool the relay does not list is left out; a tool outside the curated set is never offered, whatever the relay lists. `publish_event`, `read_management` and every management tool stay hidden.

Read tools, offered by default:

`list_repositories`, `read_repository`, `list_issues`, `read_issue`, `list_pull_requests`, `read_pull_request`, `list_files`, `read_file`, `read_attachment`, `list_rooms`, `read_room`, `read_thread`, `list_wiki`, `read_wiki_page`, `list_jobs`, `read_job`, `read_status`.

Write tools, added with `--allow-writes`:

`create_issue`, `create_pull_request`, `comment`, `post_message`, `start_thread`, `reply_in_thread`, `react`, `publish_wiki_page`, `propose_wiki_merge`, `upload_attachment`, `request_decision`, `request_grant`, `request_job`, `accept_job`, `job_progress`, `job_result`, `job_error`, `cancel_job`.

`--tools` replaces the default with the named tools, in the order given. Each name must belong to one of the two lists, and a write tool still needs `--allow-writes`; otherwise the process exits at startup with the offending name. Names the relay does not serve are accepted and simply absent from `tools/list`.

`tiny_diagnose` is the one tool the facade answers itself. It is always offered, whatever `--tools` names and whether or not the relay's table loads, and no relay tool carries the name. It takes optional `rooms` and `kinds` arrays and returns the report of `tinyagent diagnose` as `structuredContent`, with `<verdict>: <advice>` as the text block: whether the relay answers, whether it accepts the key's signature, membership and role, the grant and its state, and the named rooms and kinds the grant lacks. The verdict is `ok`, `needs-grant`, `not-a-member`, `unauthorized`, `unreachable` or `error`; a refused signature is reported as `unauthorized`, never as `unreachable`. The tool calls the relay's `browsegrant` browse method over the signed NIP-86 endpoint rather than `/mcp`, and its report is a result rather than an error whatever the verdict. See [Diagnostics](../agent-access.md#diagnostics).

The signed `browsegrant` answer is classified by its HTTP status, and the `authentication` check carries the class on failure:

| Answer | Class | Verdict | Exit |
| --- | --- | --- | --- |
| 200 with a result | none | Read from the grant: `ok`, `needs-grant` or `not-a-member` | 0 or 2 |
| 401 or 403 | `authorization` | `unauthorized`, or `not-a-member` when a 403 names a members-only relay | 2 |
| 400 whose error starts with `unsupported:` | none | `ok`; the relay predates `browsegrant`, so the signature works but the grant cannot be read | 0 |
| 429 | `rate-limited` | `unreachable`; advice says to retry later | 3 |
| Any other 4xx, including a 400 with another message | `protocol` | `error` | 3 |
| 5xx, no answer or an invalid result | `transport` | `unreachable` | 3 |

Only the `unsupported:` answer is read as an old relay. No other 4xx is ever reported as a working signature.

## Sign and resubmit

The relay never signs for a caller. A write tool called with plain fields answers with `structuredContent.unsigned`, an object with `kind`, `created_at`, `tags` and `content`, and a `next` instruction. The facade recognizes that shape on a successful result, signs the event with the agent key exactly as returned and calls the same tool again with the original arguments plus `event`. The host receives the second answer, the relay's publish result with `event_id`, `accepted` and `message`. The host never sees or signs a template.

The signature is the facade's security boundary, and a template is signed only when all three hold:

1. The process runs with `--allow-writes`.
2. The called tool is one of the write tools above.
3. The template's `kind` is one that tool is expected to publish, per this table:

| Tool | Kinds signed |
| --- | --- |
| `create_issue` | 1621 |
| `create_pull_request` | 1618 |
| `comment` | 1111 |
| `post_message` | 9 |
| `start_thread` | 11 |
| `reply_in_thread` | 12 |
| `react` | 7 |
| `publish_wiki_page` | 30818 |
| `propose_wiki_merge` | 818 |
| `upload_attachment` | none; it stores a file and publishes nothing |
| `request_decision` | 9 or 1111 |
| `request_grant` | 1111 |
| `request_job` | 43001 |
| `accept_job` | 43002 |
| `job_progress` | 43003 |
| `job_result` | 43004 |
| `job_error` | 43006 |
| `cancel_job` | 43005 |

A template that fails any of the three, including one returned by a read tool, is passed through unsigned as a tool error whose text is `the relay returned an unsigned event this tool is not expected to publish: kind <kind> from <tool>` and whose `structuredContent` is the relay's result, so the model sees what came back. The relay is not called a second time. An agent grant (kind 30392) is therefore never signed by the facade, whatever the relay answers.

Further rules:

- The event is signed as the relay returned it. The facade never edits the kind, tags, content or timestamp; a missing `created_at` is the only field it fills, with the current time.
- Kinds 22242, 24242 and 27235 are never signed, whatever the tool. A template of one of those kinds is an authorization, not a publication, and is reported as an error `refused to sign a kind <kind> authorization event returned by <tool>`.
- A template whose `tags` are not arrays of strings, or whose `kind` is not a nonnegative integer, is not recognized and is passed through unchanged so the host can see it.
- A result with `isError` is never signed.

## Errors

| Condition | Host sees |
| --- | --- |
| Relay tool result with `isError` | The same result. Text starting with `restricted:` or `auth-required:`, in the text block or in `structuredContent.message`, is prefixed with `permission: ` so an agent can answer with `request_grant`. |
| HTTP 401 or 403 from the relay | A tool error `permission: <relay message>`. |
| Connection failure, timeout, invalid response or HTTP 5xx | A tool error `transport: <detail>`. |
| Any other JSON-RPC error from the relay | A tool error `relay: <relay message>`. |
| Tool not in the exposed set | JSON-RPC error `-32602` `Unknown tool`. The relay is not contacted. |
| `tools/list` when the relay's table cannot be loaded | A table of `tiny_diagnose` alone, never a JSON-RPC error; the relay's failure, with the same prefixes as above, goes to stderr. |
| Unsigned template the facade will not sign | A tool error `the relay returned an unsigned event this tool is not expected to publish: kind <kind> from <tool>` carrying the relay's `structuredContent`. |

Errors are tool results rather than protocol errors wherever the host would show a tool result to the model.

## Bounds

Each text content block of a tool result is cut at 64 KiB on a character boundary and ends with `[truncated: <shown> of <total> bytes shown]`. `structuredContent` is bounded to 256 KiB of JSON: a larger object is replaced by `{"truncated": true, "bytes": <total>}` and the first text block gains the line `[structuredContent truncated: <total> bytes exceeds the 262144 byte limit]`, added as a new text block when the result has none. Image, audio and Base64 blocks pass through unchanged; the relay already limits `read_attachment` to 4 MiB. URLs and path addresses in results are not rewritten. Input lines up to 48 MiB are accepted.

## Hermes configuration

```yaml
mcp_servers:
  tiny:
    command: tinyagent
    args: ["mcp", "--relay", "https://relay.example.test", "--allow-writes"]
    env:
      TINY_PRIVATE_KEY: "${TINY_PRIVATE_KEY}"
```

Hermes interpolates `${VAR}` in `mcp_servers` entries from its environment, including `~/.hermes/.env`, and leaves an unset variable as the literal placeholder. A stdio server receives a filtered environment: `PATH`, `HOME`, `USER`, `LANG`, `LC_ALL`, `TERM`, `SHELL`, `TMPDIR`, `XDG_*` and the entry's `env` map. The key therefore has to be declared in `env`. `command` is resolved against that `PATH`, so the helper must be on it or named by absolute path. Hermes writes the server's stderr to its MCP log.

Sources: [`mcp.go`](../../tinyagent/mcp.go), [`client/mcp.go`](../../tinyagent/client/mcp.go), [`mcp_test.go`](../../tinyagent/mcp_test.go), [`internal/mcp/server.go`](../../internal/mcp/server.go) and [`internal/daemon/mcp_tools.go`](../../internal/daemon/mcp_tools.go).

# Custom views

A custom view renders the fenced code blocks a relay already holds. The owner names a set of event kinds and a set of fenced block languages, and points the view at an https transform they run. When a matching event arrives the relay posts the blocks in those languages to the transform, and keeps the SVG or PNG that comes back as a relay-signed artifact attached to the source event. The relay's pages then show the artifact in place of the code block.

Nothing about a view is built into the relay. It renders what its owner asked for, through a service the owner chose. A relay with no views behaves exactly as before, and a block whose artifact is missing, still queued or refused stays visible as code.

## Define a view

Call the `addcustomview` management method, or the `add_custom_view` MCP tool, as the owner:

```json
{"method": "addcustomview", "params": [{"name": "diagrams", "kinds": [30818, 30023, 30618], "transform": "https://render.example/tiny", "languages": ["mermaid", "dot", "d2"], "trigger": "write", "audience": "public"}]}
```

| Field | Meaning |
| --- | --- |
| `name` | 1 to 32 lowercase letters, digits or hyphens. Names the view and its artifact paths. |
| `kinds` | 1 to 16 event kinds to watch. Kind 30618 sends the repository README at its head. |
| `transform` | An https URL on a public host. |
| `languages` | 1 to 32 fenced block languages, lowercase. Only these blocks are sent. |
| `trigger` | `write` sends each matching event as it arrives, `hourly` sends the newest ones every hour. Defaults to `write`. |
| `audience` | `public` publishes the artifacts, `members` keeps them to members. Defaults to `public`. |
| `max_bytes` | Largest artifact accepted. 1 MiB by default, 4 MiB at most. |
| `secret` | Optional shared secret, 16 to 128 printable ASCII characters. Generated when omitted. |

The answer carries the secret once. Keep it: the transform needs it to verify each request. Only the owner may define, change or list views.

## What the relay sends

A view posts one request for each source event, carrying only the blocks:

```json
{"relay": "https://relay.example", "view": "diagrams", "source": {"id": "<source>", "kind": 30818}, "blocks": [{"index": 2, "lang": "mermaid", "source": "graph TD;\n  a-->b;"}]}
```

`index` counts every fenced block in the text from zero, whether or not it carries a language, so an answer can name the block it rendered. No event content outside the blocks, and no tags, author or timestamp, leaves the relay. Blocks in other languages are dropped before the request is built, and a block whose artifact the relay already holds is reused rather than sent again.

| Header | Value |
| --- | --- |
| `X-Tiny-View` | The view name. |
| `X-Tiny-Relay` | The relay's public URL. |
| `X-Tiny-Signature` | `sha256=` followed by the hex HMAC-SHA256 of the body under the view's secret. |

Verify the signature before rendering anything. The relay waits 30 seconds for an answer.

## What the transform answers

```json
{"artifacts": [{"block": 2, "type": "svg", "body": "<svg ...>", "engine": "mermaid"}], "errors": [{"block": 5, "error": "unknown shape on line 3"}]}
```

Each artifact names the block it rendered, its `type`, either `svg` or `png`, and its `body`, the SVG source or a base64 PNG. `engine` is optional and is recorded on the artifact. An artifact larger than the view's `max_bytes` is refused. SVG bodies are checked before they are stored: script elements, event handlers and external references are rejected rather than stripped, and the whole artifact is refused. A block listed under `errors`, or left out of the answer entirely, keeps showing its code.

## Artifacts

An artifact is a relay-signed kind 30078 record addressed `bind.ws/view/<name>/<hash>`, where the hash names the block's source text. The same block in two events produces one artifact with two sources. The record carries a `view` tag, one `block` tag for each source, a `type` tag and, when the transform reported one, an `engine` tag.

Artifacts follow their sources. An artifact expires when its last source expires, and one that outlives every source that referred to it is deleted. Editing an event detaches the blocks it no longer carries, and the artifact is dropped once nothing points at it. Removing a view deletes every artifact it produced.

Each artifact is served at `/views/<name>/<hash>.svg` or `.png` under a sandbox policy that blocks scripts and outside loads. A members-only view answers only members; a public one follows the relay's read rule. The relay's pages embed the artifact with the code block as its fallback, so a reader whose browser refuses the artifact still sees the source.

## Failures

A response outside 2xx, a timeout or a connection failure counts as a failure. The relay tries the event again after one minute and once more after five, three attempts in all, then drops it. After 20 failures in a row the view is paused and its status records the reason. A successful run resets the failure count.

## Manage views

| Method | Effect |
| --- | --- |
| `listcustomviews` | Lists views with their kinds, languages, transform host, trigger, audience, state, failure count and last run. The secret is never listed. |
| `addcustomview {name, kinds, transform, languages, trigger, audience, max_bytes, secret}` | Defines a view and returns its secret once. |
| `removecustomview <name>` | Deletes the view and every artifact it produced. |
| `pausecustomview <name>` | Stops transform requests. The artifacts stay. |
| `resumecustomview <name>` | Resumes the view and clears the failure count. |
| `runcustomview <name>` | Queues a backfill over the newest 500 events of the view's kinds. Blocks that already have an artifact are reused. |

The **Manage > Views** page lists each view with its state, last run and failure count, and buttons to pause, resume, run or remove it. The same controls are available over MCP; see [MCP](mcp.md).

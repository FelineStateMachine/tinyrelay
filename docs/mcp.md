# MCP

The relay serves a [Model Context Protocol](https://modelcontextprotocol.io/specification/2026-07-28) endpoint at `/mcp` for agents that run outside the browser. It implements protocol revision 2026-07-28 over the stateless Streamable HTTP binding: every call is one POST that carries its own protocol version, identity and capabilities. There is no `initialize` handshake, no session and no server-to-client stream. A tenant served under a path prefix has its endpoint at `/r/<name>/mcp`.

`/llms.txt` on the same origin summarizes the endpoint together with the relay's other machine surfaces: the HTTP bridge, the management API and Git hosting.

## Connecting a client

Point an MCP client that speaks revision 2026-07-28 at `https://<relay>/mcp` and give it a NIP-98 signer for the `Authorization` header. Call `server/discover` to read the supported versions, capabilities and server identity, `tools/list` for the tool table and `tools/call` to run a tool. Notifications are accepted with `202 Accepted` and no body. Responses are always `application/json`; the endpoint does not stream.

## Authorization

The MCP specification recommends OAuth 2.1 for HTTP servers. This relay uses [NIP-98](https://github.com/nostr-protocol/nips/blob/master/98.md) instead, because a Nostr key is already the identity the relay's roles, memberships and write rules are built on. Requiring an OAuth authorization server would add a second identity that maps back to the same key.

Every POST carries `Authorization: Nostr <base64 kind 27235 event>`. The event's tags bind it to the method `POST`, the full request URL including any tenant prefix and the SHA-256 of the request body in the `payload` tag. Tokens are valid for one minute and are accepted once. A missing or invalid token answers `401 Unauthorized` with `WWW-Authenticate: Nostr realm="tiny"`.

Tools execute as the signing key. Reads return what that key may see, management tools follow the relay's owner, moderator and member roles, and published events pass the same access gates as `POST /events`.

## Headers

Each request carries these headers and the matching values in the JSON-RPC body:

| Header | Value |
| --- | --- |
| `MCP-Protocol-Version` | `2026-07-28`, equal to `params._meta["io.modelcontextprotocol/protocolVersion"]` |
| `Mcp-Method` | Equal to the JSON-RPC `method` |
| `Mcp-Name` | For `tools/call`, equal to `params.name` |
| `Accept` | Includes `application/json` |
| `Content-Type` | `application/json` |
| `Origin` | Optional. When present it must be the relay's own origin |

`params._meta` also carries `io.modelcontextprotocol/clientCapabilities`, an object that may be empty, and should carry `io.modelcontextprotocol/clientInfo`. A `Mcp-Name` value that is not plain ASCII is sent as `=?base64?<value>?=` and decoded before comparison. `Mcp-Session-Id` and `Last-Event-ID` from earlier revisions are ignored.

| Condition | Response |
| --- | --- |
| GET or DELETE | `405 Method Not Allowed` with `Allow: POST` |
| `Origin` names another site | `403 Forbidden` |
| Missing or invalid NIP-98 token | `401 Unauthorized` with the `WWW-Authenticate` challenge |
| Missing `MCP-Protocol-Version`, `Mcp-Method` or `Mcp-Name`, or a header that differs from the body | `400 Bad Request`, error `-32020` HeaderMismatch |
| Protocol version the relay does not serve | `400 Bad Request`, error `-32022` UnsupportedProtocolVersion listing `supported` versions |
| Missing `_meta` fields or an unknown tool | `400 Bad Request`, error `-32602` Invalid params |
| Unknown JSON-RPC method, including `initialize` | `404 Not Found`, error `-32601` Method not found |
| Body that is not one JSON-RPC message | `400 Bad Request`, error `-32700` or `-32600` |

Tool results carry `resultType: "complete"`, a text block and `structuredContent`. Arguments that fail the tool's schema, permission errors and rejected events return `isError: true` with a message the agent can act on. `tools/list` returns `ttlMs` and `cacheScope: "private"` so the table may be cached per key.

## Tools

Read tools, which need a key that may read the relay:

| Tool | Description |
| --- | --- |
| `list_repositories` | List hosted repositories with search and pagination. |
| `read_repository` | Read a repository tree, file, history, commit diff or activity at a ref. |
| `list_issues` | List a repository's issues with search, status and label filters. |
| `read_issue` | Read an issue with its replies and authorized status changes. |
| `list_pull_requests` | List a repository's pull requests with the same filters. |
| `read_pull_request` | Read a pull request with replies, updates and the available diff. |
| `list_files` | List stored files visible to the key. |
| `read_file` | Read a stored file's metadata and preview by SHA-256 hash. |
| `read_status` | Read service health, storage and job status as an owner or moderator. |
| `read_management` | Run a read-only management method: stats, getpolicy, listaudit, listjobs, listbackups, listdumps, deliverystatus, storagestats, gitstorage, listconnections or listmembers. |
| `list_rooms` | List chat rooms visible to the key with member counts and the time of the last message. |
| `read_room` | Read a room by `id` with its members and newest messages, paged with `cursor` and `limit`. |
| `read_thread` | Read a thread root by room `id` and `event` with its replies, newest first. |
| `list_wiki` | List wiki pages with their preferred version. `q` searches titles and summaries, `author` prefers that key's versions. |
| `read_wiki_page` | Read a page by `d` with its versions, merge requests and redirects. `author` or `version` selects the version shown. |
| `read_merge_request` | Read a wiki merge request by `id` with its status, proposed version and target version. |
| `list_agents` | List granted agents with their state, scope and last event, as an owner or moderator. |

Management tools, which follow the relay's roles:

| Tool | Description |
| --- | --- |
| `run_job` | Queue an existing job to run now. |
| `add_job` | Add a pull, push, import, mirror, dump or backup job. |
| `remove_job` | Remove a job and cancel its pending runs. |
| `backup_now` | Queue a backup of relay data. |
| `dump_now` | Queue an event export. |
| `set_policy` | Apply a partial policy update as the owner. |
| `set_connections` | Replace the relay's connection list. |
| `send_test_notification` | Send the owner's test notice to the inbox and enabled devices. |
| `pause_agent` | Stop an agent until it is resumed, as an owner or moderator. |
| `resume_agent` | Let a paused agent publish again. |
| `revoke_agent` | End an agent's grant and remove its role. |
| `pause_all_agents` | Pause every active agent. |
| `resume_all_agents` | Resume every paused agent. |

Write tools, which publish through the same path as `POST /events`:

| Tool | Description |
| --- | --- |
| `publish_event` | Publish any signed Nostr event. |
| `create_issue` | Open a kind 1621 issue on a hosted repository. |
| `create_pull_request` | Open a kind 1618 pull request with its commit and clone URL. |
| `comment` | Reply to an issue, pull request or comment with a kind 1111 event carrying NIP-22 tags. |
| `set_status` | Mark an issue or pull request open, resolved, merged, closed or draft with a kind 1630 to 1633 event. |
| `post_message` | Post a kind 9 message in a room, with optional `mentions` as `p` tags. |
| `start_thread` | Start a kind 11 thread in a room with an optional title. |
| `reply_in_thread` | Reply to a thread with a kind 12 event naming the root in its `e` tag. |
| `react` | Publish a kind 7 reaction to an event: `+`, `-` or one emoji, with `room` for a room message. |
| `publish_wiki_page` | Publish or replace the key's version of a kind 30818 wiki page, with `fork_author` and `fork_event` to record a fork. |
| `propose_wiki_merge` | Ask a page's author to take in a version with a kind 818 merge request. |
| `create_room` | Create a room with a kind 9007 event carrying its id, name, description and visibility. |
| `request_decision` | Ask a person to approve, decide or answer with a kind 9 room message or a kind 1111 comment carrying a `request` tag. |

The relay never signs on a caller's behalf. Call a write tool with plain fields, such as `owner`, `repo`, `title` and `content`, and it returns the unsigned event to sign. Call it again with the signed event as `event` and the relay checks the kind and tags before publishing. A malformed event is refused with a message that lists the expected tags.

### Asking a person

`request_decision` builds an event addressed to one person. Pass `pubkey`, `request` (`approve`, `decide` or `question`) and `content`, then either `room` for a kind 9 message in that room or `root`, `root_kind` and `root_pubkey` for a kind 1111 comment under an issue, pull request or other event. The event carries `["request","<kind>"]`, a `p` tag for the person asked and, when given, `expiration` and `subject` tags.

The person answers with a kind 7 reaction to the published event from the asked key: `+` approves, `-` declines and any other content is their reply. Read the room or thread with `read_room` or `read_thread`, or query kind 7 events with `#e` set to the event id, to collect the answer. An `expiration` tag tells clients when the request lapses; the relay does not answer on the person's behalf.

## Example

A `tools/call` request for `read_management`, with the NIP-98 token abbreviated:

```
POST /mcp HTTP/1.1
Host: relay.example
Authorization: Nostr eyJpZCI6Ii4uLiIsImtpbmQiOjI3MjM1LC4uLn0=
Content-Type: application/json
Accept: application/json, text/event-stream
MCP-Protocol-Version: 2026-07-28
Mcp-Method: tools/call
Mcp-Name: read_management

{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/call",
  "params": {
    "name": "read_management",
    "arguments": {"method": "getpolicy"},
    "_meta": {
      "io.modelcontextprotocol/protocolVersion": "2026-07-28",
      "io.modelcontextprotocol/clientInfo": {"name": "example-agent", "version": "1.0"},
      "io.modelcontextprotocol/clientCapabilities": {}
    }
  }
}
```

The kind 27235 event in the `Authorization` header carries `["u","https://relay.example/mcp"]`, `["method","POST"]` and `["payload","<sha256 of the body>"]`. The response:

```
HTTP/1.1 200 OK
Content-Type: application/json

{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "resultType": "complete",
    "content": [{"type": "text", "text": "{\"result\":{\"owner\":\"...\",\"reads\":\"open\"}}"}],
    "structuredContent": {"result": {"owner": "...", "reads": "open"}},
    "isError": false,
    "_meta": {"io.modelcontextprotocol/serverInfo": {"name": "tinyrelay", "version": "..."}}
  }
}
```

## Observability

MCP requests are recorded under the `mcp` operation with the outcomes `ok`, `invalid`, `unauthorized` and `error`. Logs carry the method, tool name and outcome, never the caller's key. See [Observability](observability.md).

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gates"
	"github.com/FelineStateMachine/tinyrelay/internal/mcp"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
	"github.com/FelineStateMachine/tinyrelay/internal/wiki"
)

// The MCP tool table mirrors the browser tools in tinyclient/webmcp.js.
// Reads run through the browse methods, controls through the NIP-86
// management path, and writes through the same publish path as POST /events,
// so every tool is executed as the signed-in pubkey with the tenant's
// permission checks.

var (
	mcpText      = map[string]any{"type": "string"}
	mcpPubKey    = map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$", "description": "Hex public key."}
	mcpHash      = map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"}
	mcpID        = map[string]any{"type": "string", "minLength": 1}
	mcpLimit     = map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
	mcpCommit    = map[string]any{"type": "string", "pattern": "^[0-9a-f]{40}$", "description": "Full Git commit ID."}
	mcpEvent     = map[string]any{"type": "object", "description": "A signed Nostr event: id, pubkey, created_at, kind, tags, content and sig."}
	mcpRootKind  = map[string]any{"type": "integer", "enum": []int{1617, 1618, 1621}, "description": "Kind of the conversation root: 1621 issue, 1618 pull request, 1617 patch."}
	mcpRoomID    = map[string]any{"type": "string", "pattern": "^[a-z0-9_-]{1,64}$", "description": "Room id: 1 to 64 lowercase letters, digits, hyphen or underscore."}
	mcpPageName  = map[string]any{"type": "string", "minLength": 1, "description": "Wiki page name or title. Names are normalized: lowercase, spaces to hyphens, punctuation dropped."}
	mcpUnixTime  = map[string]any{"type": "integer", "minimum": 1, "description": "Unix time in seconds."}
	mcpJobEvent  = map[string]any{"type": "object", "description": "The kind 43001 job request event being answered; supplies e, p and room."}
	mcpAssignees = map[string]any{"type": "array", "items": mcpPubKey, "minItems": 1, "description": "Keys asked to do the work, each becoming a p tag."}
	mcpArtifacts = map[string]any{"type": "array", "items": mcp.Object(map[string]any{"type": map[string]any{"type": "string", "enum": []string{"e", "a", "r"}}, "value": map[string]any{"type": "string", "minLength": 1}}, "type", "value"), "description": "Artifacts the result references, each becoming a tag: e an event id, a an addressable event coordinate, r an http or https URL."}
	mcpReads     = map[string]any{"readOnlyHint": true, "openWorldHint": false}
	mcpChanges   = map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": false}
	mcpControls  = map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
	mcpSettings  = map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true, "openWorldHint": false}
	mcpPublishes = map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": true}

	mcpCoordinate     = regexp.MustCompile(`^30617:[0-9a-f]{64}:.+$`)
	mcpWikiCoordinate = regexp.MustCompile(`^30818:[0-9a-f]{64}:.+$`)
	mcpReadMethods    = []string{"stats", "getpolicy", "listaudit", "listjobs", "listbackups", "listdumps", "deliverystatus", "storagestats", "gitstorage", "listconnections", "listmembers"}
	mcpStatusKinds    = map[string]int{"open": 1630, "resolved": 1631, "merged": 1631, "closed": 1632, "draft": 1633}
	mcpRequestKinds   = []string{"approve", "decide", "question"}
)

const (
	mcpIssueShape      = `Expected a signed kind 1621 event with tags ["a","30617:<owner>:<repo>"], ["p","<owner>"], ["subject","<title>"] and optional ["t","<label>"] tags, with the Markdown description in content.`
	mcpPullShape       = `Expected a signed kind 1618 event with tags ["a","30617:<owner>:<repo>"], ["p","<owner>"], ["subject","<title>"], ["c","<40-hex commit>"], ["clone","<http or https URL>"], optional ["merge-base","<40-hex commit>"] and optional ["t","<label>"] tags, with the description in content.`
	mcpCommentShape    = `Expected a signed kind 1111 event with NIP-22 tags ["a","30617:<owner>:<repo>"], ["E","<root id>","","<root pubkey>"], ["K","<root kind>"], ["P","<root pubkey>"], ["e","<parent id>","","<parent pubkey>"], ["k","<parent kind>"], ["p","<parent pubkey>"], with the comment in content. The parent is the root for a top-level comment. A review comment under a pull request or patch may add ["file","<path>"] and ["line","<n>","old" or "new"] to name one diff line.`
	mcpStatusShape     = `Expected a signed event of kind 1630 (open), 1631 (resolved or merged), 1632 (closed) or 1633 (draft) with tags ["a","30617:<owner>:<repo>"], ["e","<root id>","","root"] and ["p","<root pubkey>"].`
	mcpMessageShape    = `Expected a signed kind 9 event with tag ["h","<room id>"] and optional ["p","<pubkey>"] mentions, with the message in content.`
	mcpThreadShape     = `Expected a signed kind 11 event with tag ["h","<room id>"] and optional ["subject","<title>"], with the opening post in content.`
	mcpReplyShape      = `Expected a signed kind 12 event with tags ["h","<room id>"], ["e","<thread root id>"] and optional ["p","<root author>"], with the reply in content.`
	mcpReactShape      = `Expected a signed kind 7 event with tags ["e","<event id>"], ["p","<event author>"] and, for a room message, ["h","<room id>"], with "+", "-" or one emoji in content.`
	mcpWikiShape       = `Expected a signed kind 30818 event with tags ["d","<normalized page name>"], ["title","<title>"], optional ["summary","<summary>"] and, for a fork, ["a","30818:<author>:<page name>","","fork"] and ["e","<version id>","","fork"], with the Djot article in content.`
	mcpMergeShape      = `Expected a signed kind 818 event with tags ["a","30818:<destination>:<page name>"], ["p","<destination>"], ["e","<proposed version id>","","source"] and optional ["e","<base version id>"], with the explanation in content.`
	mcpRoomShape       = `Expected a signed kind 9007 event with tags ["h","<new room id>"], ["name","<name>"], optional ["about","<description>"] and ["visibility","open" or "members"].`
	mcpRequestShape    = `Expected a signed kind 9 event with ["h","<room id>"] and optional ["e","<thread root id>","","root"], or a signed kind 1111 event with NIP-22 tags ["E","<root id>","","<root pubkey>"], ["K","<root kind>"], ["P","<root pubkey>"], ["e","<root id>","","<root pubkey>"] and ["k","<root kind>"], carrying ["request","approve", "decide" or "question"], ["p","<asked pubkey>"] and optional ["expiration","<unix time>"] and ["subject","<subject>"], with the question in content.`
	mcpJobRequestShape = `Expected a signed kind 43001 event with tags ["h","<room id>"], one or more ["p","<asked pubkey>"], optional ["subject","<title>"], ["e","<thread root id>","","root"] and ["expiration","<unix time>"], with the task text in content.`
	mcpJobAnswerShape  = `Expected a signed kind 43002 (accepted), 43003 (progress), 43004 (result) or 43006 (error) event with tags ["e","<request id>"], ["p","<requester pubkey>"] and ["h","<room id>"] matching the request, with the progress line, output or error message in content. A result may add ["e","<event id>"], ["a","<coordinate>"] and ["r","<http or https URL>"] tags for the artifacts it produced.`
	mcpJobCancelShape  = `Expected a signed kind 43005 event from the requester with tags ["e","<request id>"], ["h","<room id>"] and optional ["p","<asked pubkey>"].`
	mcpSiteShape       = `Expected a signed kind 15128 event for your own site, or a signed kind 35128 event with ["d","<site name>"] for a named site under your key, with one ["path","/<file path>","<sha256 of the file>"] tag per file, the blobs already uploaded, and an optional ["expiration","<unix time>"] tag.`
	mcpSignNext        = "Sign this event with your Nostr key and call the tool again with the signed event as the event argument."
	mcpAnswerNote      = " The asked key answers with a kind 7 reaction (+ approves, - declines) or a kind 1111 NIP-22 reply. Query kinds 7 and 1111 with #e set to the published request's event id to collect answers. A question's text answer is a reply, not an arbitrary reaction."
)

// mcpUnsigned is the event body a client signs before calling a write tool
// again.
type mcpUnsigned struct {
	Kind      int        `json:"kind"`
	CreatedAt int64      `json:"created_at"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
}

func (t *Tenant) mcpTools() (*mcp.Registry, error) {
	registry := &mcp.Registry{}
	var err error
	add := func(name, description string, schema, annotations map[string]any, handler mcp.Handler) {
		if err == nil {
			err = registry.Add(mcp.Tool{Name: name, Description: description, InputSchema: schema, Annotations: annotations, Handler: handler})
		}
	}
	repository := map[string]any{"owner": mcpPubKey, "repo": mcpID, "ref": mcpText, "path": mcpText, "view": map[string]any{"type": "string", "enum": []string{"tree", "file", "history", "commit", "activity"}}}
	collaborationList := mcp.Object(map[string]any{"owner": mcpPubKey, "repo": mcpID, "cursor": mcpText, "limit": mcpLimit, "q": mcpText, "label": mcpText, "state": map[string]any{"type": "string", "enum": []string{"open", "resolved", "merged", "closed", "draft"}}}, "owner", "repo")
	collaborationDetail := mcp.Object(map[string]any{"owner": mcpPubKey, "repo": mcpID, "event": mcpHash, "cursor": mcpText, "limit": mcpLimit}, "owner", "repo", "event")

	add("list_repositories", "List repositories visible to your account. Supports search and pagination.", mcp.Object(map[string]any{"cursor": mcpText, "limit": mcpLimit, "q": mcpText}), mcpReads, t.mcpBrowse("browserepos"))
	add("read_repository", "Read a repository tree, source file, history, commit diff or activity. Use ref to select a branch, tag or commit.", mcp.Object(merge(repository, map[string]any{"offset": map[string]any{"type": "integer", "minimum": 0}, "limit": mcpLimit}), "owner", "repo"), mcpReads, t.mcpBrowse("browserepo"))
	add("list_issues", "List repository issues with search, status filters and pagination.", collaborationList, mcpReads, t.mcpBrowse("browseissues"))
	add("read_issue", "Read an issue, its replies and authorized status changes.", collaborationDetail, mcpReads, t.mcpBrowse("browseissue"))
	add("list_pull_requests", "List repository pull requests with search, status filters and pagination.", collaborationList, mcpReads, t.mcpBrowse("browsepulls"))
	add("read_pull_request", "Read a pull request, its replies, authorized updates and available diff.", collaborationDetail, mcpReads, t.mcpBrowse("browsepull"))
	add("list_files", "List stored files visible to your account.", mcp.Object(map[string]any{"cursor": mcpText, "limit": mcpLimit, "q": mcpText}), mcpReads, t.mcpBrowse("browsefiles"))
	add("read_file", "Read a stored file's metadata and available preview by SHA-256 hash.", mcp.Object(map[string]any{"hash": mcpHash}, "hash"), mcpReads, t.mcpBrowse("browsefile"))
	add("upload_attachment", "Upload a bounded file for a room message. Returns its Blossom URL and NIP-92 metadata; pass the descriptor as an attachment to a room write tool.", mcp.Object(map[string]any{"room": mcpRoomID, "data": map[string]any{"type": "string", "description": "Standard Base64 file bytes, at most 700 KiB of encoded data. Use PUT /rooms/<id>/attachments for larger files."}, "type": map[string]any{"type": "string", "minLength": 1, "maxLength": 255}, "filename": map[string]any{"type": "string", "minLength": 1, "maxLength": 255}}, "room", "data", "type"), mcpPublishes, t.mcpUploadAttachment)
	add("read_attachment", "Read a stored attachment your key may access. Images and audio are returned as native MCP content; other files return Base64 data. Reads are limited to 4 MiB.", mcp.Object(map[string]any{"sha256": mcpHash, "max_bytes": map[string]any{"type": "integer", "minimum": 1, "maximum": 4 * 1024 * 1024}}, "sha256"), mcpReads, t.mcpReadAttachment)
	add("read_status", "Read service health, storage and job status. Requires an owner or moderator key.", mcp.Object(nil), mcpReads, t.mcpBrowse("browsestatus"))
	add("read_management", "Read relay configuration, jobs, backups, delivery status or members with your key's permissions. gitstorage requires owner and repo.", mcp.Object(map[string]any{"method": map[string]any{"type": "string", "enum": mcpReadMethods}, "owner": mcpPubKey, "repo": mcpID}, "method"), mcpReads, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		method := call.String("method")
		if method == "gitstorage" {
			if call.String("owner") == "" || call.String("repo") == "" {
				return mcp.Failure("gitstorage needs owner and repo.", nil), nil
			}
			return t.mcpManage(ctx, call, method, call.String("owner"), call.String("repo"))
		}
		return t.mcpManage(ctx, call, method)
	})
	add("list_rooms", "List chat rooms visible to your account with member counts and the time of the last message.", mcp.Object(map[string]any{"cursor": mcpText, "limit": mcpLimit}), mcpReads, t.mcpBrowse("browserooms"))
	add("read_room", "Read a room, its members and its newest messages, threads and reactions. Pass cursor from next_cursor for older messages.", mcp.Object(map[string]any{"id": mcpRoomID, "cursor": mcpText, "limit": mcpLimit}, "id"), mcpReads, t.mcpBrowse("browseroom"))
	add("read_thread", "Read a thread root and its replies in a room, newest first.", mcp.Object(map[string]any{"id": mcpRoomID, "event": mcpHash, "cursor": mcpText, "limit": mcpLimit}, "id", "event"), mcpReads, t.mcpBrowse("browsethread"))
	add("list_wiki", "List wiki pages with their preferred version. q searches titles and summaries; author prefers that key's versions.", mcp.Object(map[string]any{"q": mcpText, "author": mcpPubKey, "cursor": mcpText, "limit": mcpLimit}), mcpReads, t.mcpBrowse("browsewiki"))
	add("read_wiki_page", "Read a wiki page: its preferred version with content, every version, the history of revisions with their states, merge requests and redirects. Pass author to prefer that key's version or version to open one by event id, including an archived revision from the history.", mcp.Object(map[string]any{"d": mcpPageName, "author": mcpPubKey, "version": mcpHash}, "d"), mcpReads, t.mcpBrowse("browsewikipage"))
	add("read_merge_request", "Read a wiki merge request with its status, the proposed version and the target version.", mcp.Object(map[string]any{"id": mcpHash}, "id"), mcpReads, t.mcpBrowse("browsewikimerge"))
	add("list_agents", "List granted agents with their name, key, owner, expiration, paused and revoked state, scope and last event. Requires an owner or moderator key.", mcp.Object(nil), mcpReads, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "listagents")
	})
	add("list_jobs", "List long task requests (kind 43001) visible to your key, each with its room, requester, assignees, subject, state (open, done, failed or cancelled), status (queued, accepted, running, done, failed or cancelled), newest progress, result with its artifacts, error and cancel time. state narrows the list; mine lists only your own requests.", mcp.Object(map[string]any{"cursor": mcpText, "limit": mcpLimit, "state": map[string]any{"type": "string", "enum": jobStates}, "mine": map[string]any{"type": "boolean"}}), mcpReads, t.mcpBrowse("browsejobs"))
	add("read_job", "Read one long task request by event id with its settled state and its answers oldest first: accepted, progress, result, cancel and error events.", mcp.Object(map[string]any{"id": mcpHash}, "id"), mcpReads, t.mcpBrowse("browsejob"))
	add("list_callbacks", "List event callbacks: id, owner, host, filter, paused state, failures and last delivery. Members and agents see their own; the owner and moderators see every callback. The secret is never listed.", mcp.Object(nil), mcpReads, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "listcallbacks")
	})
	add("list_custom_views", "List the owner's custom views: name, kinds, languages, transform host, trigger, audience, state, failures and last run. The secret is never listed. Owner only.", mcp.Object(nil), mcpReads, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "listcustomviews")
	})
	add("list_join_requests", "List access requests from people who asked to join without an invite: pubkey, reason, requested_at, status (pending, approved or denied), decided_by and decided_at. Pending requests come first. Requires an owner or moderator key.", mcp.Object(nil), mcpReads, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "listjoinrequests")
	})

	add("run_job", "Queue an existing job to run now. Read jobs through read_management to check its completion.", mcp.Object(map[string]any{"id": mcpID}, "id"), mcpChanges, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "runjob", call.String("id"))
	})
	add("add_job", "Add a background job. every is the interval in hours; 0 runs once. Pull and push jobs need ws:// or wss:// relay URLs; pull may instead discover relays for a public key. Dump and backup jobs may omit relays.", mcp.Object(map[string]any{"id": mcpID, "kind": map[string]any{"type": "string", "enum": []string{"pull", "push", "import", "mirror", "dump", "backup"}}, "relays": map[string]any{"type": "array", "items": mcpText}, "filter": map[string]any{"type": "string", "description": "Nostr filter encoded as a JSON object string."}, "every": map[string]any{"type": "integer", "minimum": 0, "description": "Interval in hours. Zero runs once."}, "discoverPubKey": mcpPubKey}, "id", "kind"), mcpChanges, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "addjob", call.Arguments)
	})
	add("remove_job", "Remove a job and cancel its pending runs.", mcp.Object(map[string]any{"id": mcpID}, "id"), mcpSettings, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "removejob", call.String("id"))
	})
	add("backup_now", "Queue a backup of relay data. Read jobs and backups through read_management to check completion.", mcp.Object(nil), mcpChanges, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "backupnow")
	})
	add("dump_now", "Queue an event export. Read jobs and dumps through read_management to check completion.", mcp.Object(nil), mcpChanges, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "dumpnow")
	})
	add("set_policy", "Apply a partial relay policy update. Read the current policy before changing access, delivery or features. Requires the owner key.", mcp.Object(map[string]any{"patch": map[string]any{"type": "object", "minProperties": 1}}, "patch"), mcpSettings, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "setpolicy", call.Arguments["patch"])
	})
	add("set_connections", "Replace the relay's connection list. Read the current list with read_management listconnections before editing it.", mcp.Object(map[string]any{"connections": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}}, "connections"), mcpSettings, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "setconnections", call.Arguments["connections"])
	})
	add("send_test_notification", "Send the relay's test notice to the owner's inbox and enabled devices. Owner only.", mcp.Object(nil), mcpChanges, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "notifytest")
	})
	add("add_callback", "Register an https URL that receives a POST with each new event matching filter that your key may see. The filter takes kinds (required), authors, #a, #e, #p and #h with at most 8 values each. The answer carries the callback id and, once, the secret used for the X-Tiny-Signature header; keep it. Members and agents may hold up to the relay's callback allowance, 4 by default.", mcp.Object(map[string]any{"url": map[string]any{"type": "string", "description": "An https URL on a public host."}, "filter": map[string]any{"type": "object", "description": "NIP-01 filter subset: kinds, authors, #a, #e, #p, #h.", "minProperties": 1}, "secret": map[string]any{"type": "string", "minLength": callbackSecretMin, "maxLength": callbackSecretMax, "description": "Optional shared secret, 16 to 128 printable ASCII characters. Generated when omitted."}}, "url", "filter"), mcpChanges, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "addcallback", call.Arguments)
	})
	add("add_custom_view", "Define a custom view: the relay POSTs the fenced code blocks of matching events, in the listed languages, to an https transform and keeps the SVG or PNG artifacts it returns as relay-signed events shown in place of the blocks. The answer carries the secret once, used for the X-Transform-Signature header; keep it. Owner only.", mcp.Object(map[string]any{"name": map[string]any{"type": "string", "pattern": "^[a-z0-9-]{1,32}$", "description": "View name: 1 to 32 lowercase letters, digits or hyphens."}, "kinds": map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 0, "maximum": 65535}, "minItems": 1, "maxItems": viewKindMax, "description": "Event kinds the view watches; 30618 sends the repository README at its head."}, "transform": map[string]any{"type": "string", "description": "An https URL on a public host that renders the blocks."}, "trigger": map[string]any{"type": "string", "enum": []string{"write", "hourly"}, "description": "write sends each matching event as it arrives; hourly sends the newest ones every hour."}, "audience": map[string]any{"type": "string", "enum": []string{"public", "members"}, "description": "Who may see the artifacts."}, "languages": map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1, "maxLength": viewLanguageLength}, "minItems": 1, "maxItems": viewLanguageMax, "description": "Fenced block languages the view renders, lowercase."}, "max_bytes": map[string]any{"type": "integer", "minimum": 1, "maximum": viewMaxBytesCap, "description": "Largest artifact accepted, in bytes. 1 MiB when omitted, 4 MiB at most."}, "secret": map[string]any{"type": "string", "minLength": callbackSecretMin, "maxLength": callbackSecretMax, "description": "Optional shared secret, 16 to 128 printable ASCII characters. Generated when omitted."}}, "name", "kinds", "transform", "languages"), mcpChanges, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "addcustomview", call.Arguments)
	})
	add("remove_custom_view", "Delete a custom view and every artifact it produced. Owner only.", mcp.Object(map[string]any{"name": mcpID}, "name"), mcpSettings, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "removecustomview", call.String("name"))
	})
	add("pause_custom_view", "Stop a custom view: no transform requests until it is resumed. Its artifacts stay. Owner only.", mcp.Object(map[string]any{"name": mcpID}, "name"), mcpControls, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "pausecustomview", call.String("name"))
	})
	add("resume_custom_view", "Resume a paused custom view and clear its failure count. Owner only.", mcp.Object(map[string]any{"name": mcpID}, "name"), mcpControls, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "resumecustomview", call.String("name"))
	})
	add("run_custom_view", "Queue a backfill of a custom view over the newest 500 events of its kinds. Blocks that already have an artifact are reused unless refresh is true, which rebuilds them from the transform. Owner only.", mcp.Object(map[string]any{"name": mcpID, "refresh": map[string]any{"type": "boolean", "description": "Rebuild existing artifacts from the transform."}}, "name"), mcpChanges, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "runcustomview", call.Arguments)
	})
	add("remove_callback", "Delete a callback by id. The callback's owner, the relay owner and moderators may do this.", mcp.Object(map[string]any{"id": mcpID}, "id"), mcpSettings, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "removecallback", call.String("id"))
	})
	add("pause_callback", "Stop deliveries to a callback until it is resumed. The callback's owner, the relay owner and moderators may do this.", mcp.Object(map[string]any{"id": mcpID}, "id"), mcpControls, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "pausecallback", call.String("id"))
	})
	add("resume_callback", "Resume a paused callback and clear its failure count.", mcp.Object(map[string]any{"id": mcpID}, "id"), mcpControls, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "resumecallback", call.String("id"))
	})
	add("pause_agent", "Stop an agent from publishing until it is resumed. The grant stays in place. Requires an owner or moderator key.", mcp.Object(map[string]any{"agent": mcpPubKey}, "agent"), mcpControls, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "pauseagent", call.String("agent"))
	})
	add("resume_agent", "Let a paused agent publish again. Requires an owner or moderator key.", mcp.Object(map[string]any{"agent": mcpPubKey}, "agent"), mcpControls, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "resumeagent", call.String("agent"))
	})
	add("revoke_agent", "End an agent's grant and remove its agent role. The grant event stays stored for audit; a fresh grant restores access. Requires an owner or moderator key.", mcp.Object(map[string]any{"agent": mcpPubKey}, "agent"), mcpSettings, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "revokeagent", call.String("agent"))
	})
	add("approve_join", "Approve an access request: the key becomes a member, the relay's member list and add-user record follow, and the request is marked approved. Requires an owner or moderator key.", mcp.Object(map[string]any{"pubkey": mcpPubKey}, "pubkey"), mcpControls, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "approvejoin", call.String("pubkey"))
	})
	add("deny_join", "Deny an access request. The key stays outside the relay and may ask again. Requires an owner or moderator key.", mcp.Object(map[string]any{"pubkey": mcpPubKey}, "pubkey"), mcpControls, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "denyjoin", call.String("pubkey"))
	})
	add("pause_all_agents", "Pause every active agent at once. Requires an owner or moderator key.", mcp.Object(nil), mcpControls, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "pauseallagents")
	})
	add("resume_all_agents", "Resume every paused agent. Requires an owner or moderator key.", mcp.Object(nil), mcpControls, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "resumeallagents")
	})

	add("publish_event", "Publish a signed Nostr event to this relay. The relay's access rules apply as they do for POST /events.", mcp.Object(map[string]any{"event": mcpEvent}, "event"), mcpPublishes, t.mcpWrite(nil, nil, "Expected a signed Nostr event."))
	add("create_issue", "Open an issue on a hosted repository. Pass owner, repo, title and content to receive the unsigned kind 1621 event, sign it, then call again with the signed event.", mcp.Object(map[string]any{"event": mcpEvent, "owner": mcpPubKey, "repo": mcpID, "title": mcpText, "content": mcpText, "labels": map[string]any{"type": "array", "items": mcpText}}), mcpPublishes, t.mcpWrite(mcpBuildIssue, mcpCheckIssue, mcpIssueShape))
	add("create_pull_request", "Open a pull request on a hosted repository. Pass owner, repo, title, commit and clone (plus optional content, merge_base and labels) to receive the unsigned kind 1618 event, sign it, then call again with the signed event.", mcp.Object(map[string]any{"event": mcpEvent, "owner": mcpPubKey, "repo": mcpID, "title": mcpText, "content": mcpText, "commit": mcpCommit, "clone": map[string]any{"type": "string", "description": "HTTP or HTTPS clone URL without credentials."}, "merge_base": mcpCommit, "labels": map[string]any{"type": "array", "items": mcpText}}), mcpPublishes, t.mcpWrite(mcpBuildPull, mcpCheckPull, mcpPullShape))
	add("comment", "Reply to an issue, pull request or comment with a NIP-22 kind 1111 event. Pass owner, repo, root, root_kind, root_pubkey and content (plus parent, parent_kind and parent_pubkey to answer a comment) to receive the unsigned event, sign it, then call again with the signed event. To review one line of a pull request or patch diff, add file, line and side (old for the base, new for the change).", mcp.Object(map[string]any{"event": mcpEvent, "owner": mcpPubKey, "repo": mcpID, "root": mcpHash, "root_kind": mcpRootKind, "root_pubkey": mcpPubKey, "parent": mcpHash, "parent_kind": map[string]any{"type": "integer", "enum": []int{1111, 1617, 1618, 1621}}, "parent_pubkey": mcpPubKey, "content": mcpText, "file": map[string]any{"type": "string", "minLength": 1, "description": "Path of the diff file the comment is about."}, "line": map[string]any{"type": "integer", "minimum": 1, "description": "Line number on the chosen side of the diff."}, "side": map[string]any{"type": "string", "enum": []string{"old", "new"}}}), mcpPublishes, t.mcpWrite(mcpBuildComment, mcpCheckComment, mcpCommentShape))
	add("set_status", "Change the status of an issue or pull request. The author, repository owner and maintainers may do this. Pass owner, repo, root, root_pubkey and status to receive the unsigned event, sign it, then call again with the signed event.", mcp.Object(map[string]any{"event": mcpEvent, "owner": mcpPubKey, "repo": mcpID, "root": mcpHash, "root_pubkey": mcpPubKey, "status": map[string]any{"type": "string", "enum": []string{"open", "resolved", "merged", "closed", "draft"}}}), mcpPublishes, t.mcpWrite(mcpBuildStatus, mcpCheckStatus, mcpStatusShape))
	attachments := map[string]any{"type": "array", "items": mcpAttachment, "maxItems": 8}
	add("post_message", "Post a kind 9 chat message in a room. Pass room and content or attachments, plus optional mentions, to receive an unsigned event. Sign it, then call again with event. Posting in an open room joins it.", mcp.Object(map[string]any{"event": mcpEvent, "room": mcpRoomID, "content": mcpText, "mentions": map[string]any{"type": "array", "items": mcpPubKey}, "attachments": attachments}), mcpPublishes, t.mcpWrite(mcpBuildMessage, mcpCheckMessage, mcpMessageShape))
	add("start_thread", "Start a kind 11 thread in a room. Pass room and content or attachments, plus an optional title, to receive an unsigned event. Sign it, then call again with event.", mcp.Object(map[string]any{"event": mcpEvent, "room": mcpRoomID, "title": mcpText, "content": mcpText, "attachments": attachments}), mcpPublishes, t.mcpWrite(mcpBuildThread, mcpCheckThread, mcpThreadShape))
	add("reply_in_thread", "Reply with a kind 12 event. Pass room, root and content or attachments, plus optional root_pubkey, to receive an unsigned event. A nested root is resolved to the thread root before signing. Sign it, then call again with event.", mcp.Object(map[string]any{"event": mcpEvent, "room": mcpRoomID, "root": mcpHash, "root_pubkey": mcpPubKey, "content": mcpText, "attachments": attachments}), mcpPublishes, t.mcpWrite(mcpBuildReply, mcpCheckReply, mcpReplyShape))
	add("react", "React to an event with a kind 7 reaction: + to like or approve, - to dislike or decline, or one emoji. Pass target, target_pubkey and content (plus room for a room message) to receive the unsigned event, sign it, then call again with the signed event. A + or - from a wiki merge request's destination author answers the request.", mcp.Object(map[string]any{"event": mcpEvent, "target": mcpHash, "target_pubkey": mcpPubKey, "content": map[string]any{"type": "string", "minLength": 1, "description": "+, - or one emoji."}, "room": mcpRoomID}), mcpPublishes, t.mcpWrite(mcpBuildReact, mcpCheckReact, mcpReactShape))
	add("publish_wiki_page", "Publish or replace your version of a kind 30818 wiki page in Djot markup. Pass d (the page name), title and content, plus optional summary and, to fork another author's version, fork_author and fork_event, to receive the unsigned event, sign it, then call again with the signed event.", mcp.Object(map[string]any{"event": mcpEvent, "d": mcpPageName, "title": mcpText, "summary": mcpText, "content": mcpText, "fork_author": mcpPubKey, "fork_event": mcpHash}), mcpPublishes, t.mcpWrite(mcpBuildWikiPage, mcpCheckWikiPage, mcpWikiShape))
	add("propose_wiki_merge", "Ask a wiki author to take in changes from another version with a kind 818 merge request. Pass d, destination (the author asked), source (the proposed version's event id) and content, plus an optional base version id, to receive the unsigned event, sign it, then call again with the signed event. The destination author answers with a + or - reaction.", mcp.Object(map[string]any{"event": mcpEvent, "d": mcpPageName, "destination": mcpPubKey, "source": mcpHash, "base": mcpHash, "content": mcpText}), mcpPublishes, t.mcpWrite(mcpBuildWikiMerge, mcpCheckWikiMerge, mcpMergeShape))
	add("publish_site", "Publish a static site manifest (NIP-5A). Upload the files to the blob store first, then pass paths as [path, sha256] pairs, an optional label (your npub for your own site, the default, or a named site label under your key) and an optional expiration, to receive the unsigned kind 15128 or 35128 event, sign it, then call again with the signed event. An agent needs a sites grant that covers the label; a grant with a ttl requires the expiration.", mcp.Object(map[string]any{"event": mcpEvent, "label": map[string]any{"type": "string", "minLength": 1, "description": "Site label: your npub, or a named site label under your key. Defaults to your own site."}, "paths": map[string]any{"type": "array", "items": map[string]any{"type": "array", "items": mcpText}, "description": "One [path, sha256] pair per file, such as [\"/index.html\", \"<sha256>\"]."}, "expiration": mcpUnixTime}), mcpPublishes, t.mcpWrite(mcpBuildSite, mcpCheckSite, mcpSiteShape))
	add("create_room", "Create a chat room with a kind 9007 event. Relay members may do this. Pass room (the new id), name and optional about and visibility (open or members) to receive the unsigned event, sign it, then call again with the signed event. The signer becomes the room owner.", mcp.Object(map[string]any{"event": mcpEvent, "room": mcpRoomID, "name": mcpText, "about": mcpText, "visibility": map[string]any{"type": "string", "enum": []string{"open", "members"}}}), mcpPublishes, t.mcpWrite(mcpBuildRoom, mcpCheckRoom, mcpRoomShape))
	add("request_decision", "Ask a person for an approval, a decision or an answer. Pass pubkey, request and content, plus room and optional root for a kind 9 room message, or root, root_kind and root_pubkey for a kind 1111 comment under another event. A room root is resolved to the thread root before signing."+mcpAnswerNote, mcp.Object(map[string]any{"event": mcpEvent, "pubkey": mcpPubKey, "request": map[string]any{"type": "string", "enum": mcpRequestKinds}, "content": mcpText, "room": mcpRoomID, "root": mcpHash, "root_kind": map[string]any{"type": "integer", "minimum": 0}, "root_pubkey": mcpPubKey, "expiration": mcpUnixTime, "subject": mcpText}), mcpPublishes, t.mcpWrite(mcpBuildRequest, mcpCheckRequest, mcpRequestShape))
	add("request_job", "Ask one or more keys to take on a long task with a kind 43001 job request in a room. Pass room, assignees and content or subject, plus optional root (a thread root in the room) and expiration. The keys asked answer with accept_job, job_progress and job_result or job_error; cancel_job withdraws the request; read_job follows it.", mcp.Object(map[string]any{"event": mcpEvent, "room": mcpRoomID, "assignees": mcpAssignees, "subject": mcpText, "content": mcpText, "root": mcpHash, "expiration": mcpUnixTime}), mcpPublishes, t.mcpWrite(mcpBuildJobRequest, mcpCheckJobRequest, mcpJobRequestShape))
	add("accept_job", "Take on a long task you were asked to do with a kind 43002 job accepted event. Pass request (the job request event) or e, p and room, plus optional content.", mcpJobAnswerSchema(false), mcpPublishes, t.mcpWrite(mcpBuildJobAnswer(event.KIND_JOB_ACCEPTED), mcpCheckJobAnswer(event.KIND_JOB_ACCEPTED), mcpJobAnswerShape))
	add("job_progress", "Report progress on a long task you were asked to do with a kind 43003 job progress event, as often as needed. Pass request or e, p and room, plus content with the progress line.", mcpJobAnswerSchema(false), mcpPublishes, t.mcpWrite(mcpBuildJobAnswer(event.KIND_JOB_PROGRESS), mcpCheckJobAnswer(event.KIND_JOB_PROGRESS), mcpJobAnswerShape))
	add("job_result", "Finish a long task with a kind 43004 job result. Pass request or e, p and room, content with the output and optional artifacts, each an e, a or r reference that clients link.", mcpJobAnswerSchema(true), mcpPublishes, t.mcpWrite(mcpBuildJobAnswer(event.KIND_JOB_RESULT), mcpCheckJobAnswer(event.KIND_JOB_RESULT), mcpJobAnswerShape))
	add("job_error", "End a long task that failed with a kind 43006 job error. Pass request or e, p and room, plus content with the error message.", mcpJobAnswerSchema(false), mcpPublishes, t.mcpWrite(mcpBuildJobAnswer(event.KIND_JOB_ERROR), mcpCheckJobAnswer(event.KIND_JOB_ERROR), mcpJobAnswerShape))
	add("cancel_job", "Cancel a long task you requested with a kind 43005 job cancel. Pass request or e and room, plus optional p (one of the keys asked) and content.", mcp.Object(map[string]any{"event": mcpEvent, "request": mcpJobEvent, "e": mcpHash, "room": mcpRoomID, "p": mcpPubKey, "content": mcpText}), mcpPublishes, t.mcpWrite(mcpBuildJobCancel, mcpCheckJobCancel, mcpJobCancelShape))
	add("request_grant", "Ask your current grant operator for agent permission changes. Pass reason (1 to 500 Unicode characters) and changes. Kinds and rooms are added; repos and sites replace matching entries; wiki, jobs and rate replace their fields. Other fields stay unchanged. Sign the returned kind 1111 request and call again with event. Only the operator's signed kind 30392 replacement grants access; a + reaction does not.", mcpGrantRequestSchema(), mcpPublishes, t.mcpGrantRequest)
	return registry, err
}

func (t *Tenant) mcpBrowse(method string) mcp.Handler {
	return func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		raw, err := json.Marshal(call.Arguments)
		if err != nil {
			return mcp.Result{}, err
		}
		result, err := t.Execute(ctx, call.Actor, method, []json.RawMessage{raw})
		if err != nil {
			return mcp.Result{}, err
		}
		return mcp.Value(result), nil
	}
}

func (t *Tenant) mcpManage(ctx context.Context, call mcp.Call, method string, params ...any) (mcp.Result, error) {
	raw := make([]json.RawMessage, 0, len(params))
	for _, param := range params {
		encoded, err := json.Marshal(param)
		if err != nil {
			return mcp.Result{}, err
		}
		raw = append(raw, encoded)
	}
	result, err := t.Execute(ctx, call.Actor, method, raw)
	if err != nil {
		return mcp.Result{}, err
	}
	return mcp.Value(map[string]any{"result": result}), nil
}

// mcpWrite serves a write tool. A signed event argument is checked against
// the expected shape and published; without one the builder returns the
// unsigned event for the client to sign.
func (t *Tenant) mcpWrite(build func(mcp.Call) (mcpUnsigned, error), check func(event.Event) error, shape string) mcp.Handler {
	return func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		if raw, present := call.Arguments["event"]; present && raw != nil {
			encoded, err := json.Marshal(raw)
			if err != nil {
				return mcp.Result{}, err
			}
			e, err := event.Parse(encoded)
			if err == nil && check != nil {
				err = check(e)
			}
			if err != nil {
				return mcp.Failure("The event is not acceptable: "+err.Error()+"\n"+shape, map[string]any{"expected": shape}), nil
			}
			return t.mcpPublish(ctx, call, e), nil
		}
		if build == nil {
			return mcp.Failure("event is required. "+shape, map[string]any{"expected": shape}), nil
		}
		unsigned, err := build(call)
		if err != nil {
			return mcp.Failure(err.Error()+"\n"+shape, map[string]any{"expected": shape}), nil
		}
		unsigned, err = t.canonicalizeRoomThreadUnsigned(ctx, call.Actor, unsigned)
		if err != nil {
			return mcp.Failure(err.Error()+"\n"+shape, map[string]any{"expected": shape}), nil
		}
		return mcp.Value(map[string]any{"unsigned": unsigned, "next": mcpSignNext}), nil
	}
}

// canonicalizeRoomThreadUnsigned keeps room replies at one visible level.
// The event is still unsigned, so its parent reference can be normalized
// before the client signs it. Signed events are intentionally checked as
// submitted because changing them would invalidate their signatures.
func (t *Tenant) canonicalizeRoomThreadUnsigned(ctx context.Context, actor string, unsigned mcpUnsigned) (mcpUnsigned, error) {
	if unsigned.Kind != event.KIND_THREAD_REPLY && unsigned.Kind != event.KIND_CHAT {
		return unsigned, nil
	}
	room := event.Tag(event.Event{Tags: unsigned.Tags}, "h")
	rootID := event.Tag(event.Event{Tags: unsigned.Tags}, "e")
	if room == "" || rootID == "" {
		return unsigned, nil
	}
	root, err := t.roomThreadRoot(ctx, actor, room, rootID)
	if err != nil {
		return mcpUnsigned{}, err
	}
	for _, tag := range unsigned.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			tag[1] = root.ID
			break
		}
	}
	if unsigned.Kind == event.KIND_THREAD_REPLY {
		for _, tag := range unsigned.Tags {
			if len(tag) >= 2 && tag[0] == "p" {
				tag[1] = root.PubKey
				break
			}
		}
	}
	return unsigned, nil
}

func (t *Tenant) mcpPublish(ctx context.Context, call mcp.Call, e event.Event) mcp.Result {
	s := relay.Session{RelayURL: t.RelayURL(), PubKeys: []string{call.Actor}}
	if call.Request != nil {
		if ip, _, err := net.SplitHostPort(call.Request.RemoteAddr); err == nil {
			s.RemoteIP = ip
		} else {
			s.RemoteIP = call.Request.RemoteAddr
		}
	}
	message, err := t.router.Publish(ctx, e, s)
	if err != nil {
		return mcp.Failure("The relay rejected the event: "+err.Error(), map[string]any{"event_id": e.ID, "accepted": false, "message": err.Error()})
	}
	return mcp.Value(map[string]any{"event_id": e.ID, "accepted": true, "message": message})
}

func mcpRepositoryTags(call mcp.Call) (string, string, [][]string, error) {
	owner, repo := call.String("owner"), call.String("repo")
	if owner == "" || repo == "" {
		return "", "", nil, errors.New("owner and repo are required")
	}
	coordinate := "30617:" + owner + ":" + repo
	return owner, coordinate, [][]string{{"a", coordinate}}, nil
}

func mcpLabels(call mcp.Call, tags [][]string) [][]string {
	seen := map[string]bool{}
	if labels, ok := call.Arguments["labels"].([]any); ok {
		for _, label := range labels {
			text, _ := label.(string)
			text = strings.TrimSpace(text)
			if text != "" && !seen[text] {
				seen[text] = true
				tags = append(tags, []string{"t", text})
			}
		}
	}
	return tags
}

func mcpBuildIssue(call mcp.Call) (mcpUnsigned, error) {
	owner, _, tags, err := mcpRepositoryTags(call)
	if err != nil {
		return mcpUnsigned{}, err
	}
	title, content := strings.TrimSpace(call.String("title")), call.String("content")
	if title == "" || strings.TrimSpace(content) == "" {
		return mcpUnsigned{}, errors.New("title and content are required")
	}
	tags = mcpLabels(call, append(tags, []string{"p", owner}, []string{"subject", title}))
	return mcpUnsigned{Kind: event.KIND_GIT_ISSUE, CreatedAt: time.Now().Unix(), Tags: tags, Content: content}, nil
}

func mcpBuildPull(call mcp.Call) (mcpUnsigned, error) {
	owner, _, tags, err := mcpRepositoryTags(call)
	if err != nil {
		return mcpUnsigned{}, err
	}
	title, commit, clone, base := strings.TrimSpace(call.String("title")), call.String("commit"), call.String("clone"), call.String("merge_base")
	if title == "" || commit == "" || clone == "" {
		return mcpUnsigned{}, errors.New("title, commit and clone are required")
	}
	if err := mcpCloneURL(clone); err != nil {
		return mcpUnsigned{}, err
	}
	tags = append(tags, []string{"p", owner}, []string{"subject", title}, []string{"c", commit}, []string{"clone", clone})
	if base != "" {
		tags = append(tags, []string{"merge-base", base})
	}
	return mcpUnsigned{Kind: event.KIND_GIT_PR, CreatedAt: time.Now().Unix(), Tags: mcpLabels(call, tags), Content: call.String("content")}, nil
}

func mcpBuildComment(call mcp.Call) (mcpUnsigned, error) {
	_, _, tags, err := mcpRepositoryTags(call)
	if err != nil {
		return mcpUnsigned{}, err
	}
	root, rootPubKey, content := call.String("root"), call.String("root_pubkey"), call.String("content")
	rootKind := call.Int("root_kind")
	if root == "" || rootPubKey == "" || rootKind == 0 || strings.TrimSpace(content) == "" {
		return mcpUnsigned{}, errors.New("root, root_kind, root_pubkey and content are required")
	}
	parent, parentPubKey, parentKind := call.String("parent"), call.String("parent_pubkey"), call.Int("parent_kind")
	if parent == "" {
		parent, parentPubKey, parentKind = root, rootPubKey, rootKind
	}
	if parentPubKey == "" || parentKind == 0 {
		return mcpUnsigned{}, errors.New("parent needs parent_kind and parent_pubkey")
	}
	tags = append(tags, []string{"E", root, "", rootPubKey}, []string{"K", strconv.Itoa(rootKind)}, []string{"P", rootPubKey}, []string{"e", parent, "", parentPubKey}, []string{"k", strconv.Itoa(parentKind)}, []string{"p", parentPubKey})
	file, line, side := call.String("file"), call.Int("line"), call.String("side")
	if file != "" || line != 0 || side != "" {
		if strings.TrimSpace(file) == "" || line < 1 || (side != "old" && side != "new") {
			return mcpUnsigned{}, errors.New("file, line (a positive integer) and side (old or new) go together")
		}
		if rootKind != event.KIND_GIT_PR && rootKind != event.KIND_GIT_PATCH {
			return mcpUnsigned{}, errors.New("file and line anchor a comment to a pull request or patch diff")
		}
		tags = append(tags, []string{"file", file}, []string{"line", strconv.Itoa(line), side})
	}
	return mcpUnsigned{Kind: 1111, CreatedAt: time.Now().Unix(), Tags: tags, Content: content}, nil
}

func mcpBuildStatus(call mcp.Call) (mcpUnsigned, error) {
	owner, _, tags, err := mcpRepositoryTags(call)
	if err != nil {
		return mcpUnsigned{}, err
	}
	root, rootPubKey, status := call.String("root"), call.String("root_pubkey"), call.String("status")
	kind, known := mcpStatusKinds[status]
	if root == "" || rootPubKey == "" || !known {
		return mcpUnsigned{}, errors.New("root, root_pubkey and status are required")
	}
	tags = append(tags, []string{"e", root, "", "root"}, []string{"p", rootPubKey})
	if owner != rootPubKey {
		tags = append(tags, []string{"p", owner})
	}
	return mcpUnsigned{Kind: kind, CreatedAt: time.Now().Unix(), Tags: tags, Content: ""}, nil
}

func mcpCoordinateOwner(e event.Event) (string, error) {
	coordinate := event.Tag(e, "a")
	if !mcpCoordinate.MatchString(coordinate) {
		return "", errors.New(`the a tag must name the repository as 30617:<owner>:<repo>`)
	}
	return strings.Split(coordinate, ":")[1], nil
}

func mcpCheckIssue(e event.Event) error {
	if e.Kind != event.KIND_GIT_ISSUE {
		return fmt.Errorf("kind %d is not an issue", e.Kind)
	}
	return mcpCheckRoot(e)
}

func mcpCheckPull(e event.Event) error {
	if e.Kind != event.KIND_GIT_PR {
		return fmt.Errorf("kind %d is not a pull request", e.Kind)
	}
	if err := mcpCheckRoot(e); err != nil {
		return err
	}
	if !hex40(event.Tag(e, "c")) || (event.Tag(e, "merge-base") != "" && !hex40(event.Tag(e, "merge-base"))) {
		return errors.New("the c tag (and merge-base, when present) must be a full 40-hex commit ID")
	}
	return mcpCloneURL(event.Tag(e, "clone"))
}

func mcpCheckRoot(e event.Event) error {
	owner, err := mcpCoordinateOwner(e)
	if err != nil {
		return err
	}
	if !contains(event.TagValues(e, "p"), owner) {
		return errors.New("a p tag must name the repository owner")
	}
	if strings.TrimSpace(event.Tag(e, "subject")) == "" {
		return errors.New("the subject tag carries the title and must not be empty")
	}
	if e.Kind == event.KIND_GIT_ISSUE && strings.TrimSpace(e.Content) == "" {
		return errors.New("content must not be empty")
	}
	return nil
}

func mcpCheckComment(e event.Event) error {
	if e.Kind != 1111 {
		return fmt.Errorf("kind %d is not a comment", e.Kind)
	}
	if _, err := mcpCoordinateOwner(e); err != nil {
		return err
	}
	rootKind := event.Tag(e, "K")
	if !hex64(event.Tag(e, "E")) || !hex64(event.Tag(e, "P")) || !contains([]string{"1617", "1618", "1621"}, rootKind) {
		return errors.New("the E, K and P tags must name the root event, its kind (1617, 1618 or 1621) and its author")
	}
	if !hex64(event.Tag(e, "e")) || !hex64(event.Tag(e, "p")) || !contains([]string{"1111", rootKind}, event.Tag(e, "k")) {
		return errors.New("the e, k and p tags must name the parent event, its kind and its author")
	}
	if strings.TrimSpace(e.Content) == "" {
		return errors.New("content must not be empty")
	}
	if err := validateReviewAnchor(e); err != nil {
		return errors.New(strings.TrimPrefix(err.Error(), "blocked: "))
	}
	return nil
}

func mcpCheckStatus(e event.Event) error {
	if e.Kind < 1630 || e.Kind > 1633 {
		return fmt.Errorf("kind %d is not a status", e.Kind)
	}
	if _, err := mcpCoordinateOwner(e); err != nil {
		return err
	}
	if !hex64(event.Tag(e, "e")) || !hex64(event.Tag(e, "p")) {
		return errors.New("the e and p tags must name the issue or pull request and its author")
	}
	return nil
}

func mcpCloneURL(raw string) error {
	clone, err := url.Parse(raw)
	if err != nil || (clone.Scheme != "http" && clone.Scheme != "https") || clone.Host == "" || clone.User != nil || clone.Fragment != "" {
		return errors.New("clone must be an HTTP or HTTPS URL without credentials")
	}
	return nil
}

func hex64(value string) bool { return len(value) == 64 && isHexLower(value) }
func hex40(value string) bool { return len(value) == 40 && isHexLower(value) }
func isHexLower(value string) bool {
	return value != "" && strings.Trim(value, "0123456789abcdef") == ""
}

func merge(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range extra {
		out[key] = value
	}
	return out
}

// Room, wiki and decision builders. Each returns the unsigned event for the
// caller to sign; the matching check accepts the signed event back.

func mcpRoomTag(call mcp.Call) ([][]string, error) {
	room := call.String("room")
	if !community.ValidRoomID(room) {
		return nil, errors.New("room is required and must be a room id")
	}
	return [][]string{{"h", room}}, nil
}

func mcpPubKeys(call mcp.Call, key string, tags [][]string) [][]string {
	seen := map[string]bool{}
	if values, ok := call.Arguments[key].([]any); ok {
		for _, value := range values {
			text, _ := value.(string)
			if hex64(text) && !seen[text] {
				seen[text] = true
				tags = append(tags, []string{"p", text})
			}
		}
	}
	return tags
}

func mcpBuildMessage(call mcp.Call) (mcpUnsigned, error) {
	tags, err := mcpRoomTag(call)
	if err != nil {
		return mcpUnsigned{}, err
	}
	content, attachmentTags, err := mcpRoomContent(call)
	if err != nil {
		return mcpUnsigned{}, err
	}
	if strings.TrimSpace(content) == "" {
		return mcpUnsigned{}, errors.New("content or attachments is required")
	}
	tags = append(tags, attachmentTags...)
	return mcpUnsigned{Kind: event.KIND_CHAT, CreatedAt: time.Now().Unix(), Tags: mcpPubKeys(call, "mentions", tags), Content: content}, nil
}

func mcpBuildThread(call mcp.Call) (mcpUnsigned, error) {
	tags, err := mcpRoomTag(call)
	if err != nil {
		return mcpUnsigned{}, err
	}
	title, content := strings.TrimSpace(call.String("title")), call.String("content")
	content, attachmentTags, err := mcpRoomContent(call)
	if err != nil {
		return mcpUnsigned{}, err
	}
	if strings.TrimSpace(content) == "" {
		return mcpUnsigned{}, errors.New("content or attachments is required")
	}
	if title != "" {
		tags = append(tags, []string{"subject", title})
	}
	tags = append(tags, attachmentTags...)
	return mcpUnsigned{Kind: event.KIND_THREAD, CreatedAt: time.Now().Unix(), Tags: tags, Content: content}, nil
}

func mcpBuildReply(call mcp.Call) (mcpUnsigned, error) {
	tags, err := mcpRoomTag(call)
	if err != nil {
		return mcpUnsigned{}, err
	}
	root, rootPubKey := call.String("root"), call.String("root_pubkey")
	content, attachmentTags, err := mcpRoomContent(call)
	if root == "" || strings.TrimSpace(content) == "" {
		return mcpUnsigned{}, errors.New("root and content or attachments are required")
	}
	tags = append(tags, []string{"e", root})
	if rootPubKey != "" {
		tags = append(tags, []string{"p", rootPubKey})
	}
	tags = append(tags, attachmentTags...)
	return mcpUnsigned{Kind: event.KIND_THREAD_REPLY, CreatedAt: time.Now().Unix(), Tags: tags, Content: content}, nil
}

func mcpBuildReact(call mcp.Call) (mcpUnsigned, error) {
	target, targetPubKey, content, room := call.String("target"), call.String("target_pubkey"), call.String("content"), call.String("room")
	if target == "" || targetPubKey == "" || content == "" {
		return mcpUnsigned{}, errors.New("target, target_pubkey and content are required")
	}
	if !mcpReaction(content) {
		return mcpUnsigned{}, errors.New("content must be +, - or one emoji")
	}
	tags := [][]string{{"e", target}, {"p", targetPubKey}}
	if room != "" {
		tags = append(tags, []string{"h", room})
	}
	return mcpUnsigned{Kind: 7, CreatedAt: time.Now().Unix(), Tags: tags, Content: content}, nil
}

func mcpBuildWikiPage(call mcp.Call) (mcpUnsigned, error) {
	d, title, content := wiki.Normalize(call.String("d")), strings.TrimSpace(call.String("title")), call.String("content")
	if d == "" || title == "" || strings.TrimSpace(content) == "" {
		return mcpUnsigned{}, errors.New("d, title and content are required")
	}
	tags := [][]string{{"d", d}, {"title", title}}
	if summary := strings.TrimSpace(call.String("summary")); summary != "" {
		tags = append(tags, []string{"summary", summary})
	}
	forkAuthor, forkEvent := call.String("fork_author"), call.String("fork_event")
	if (forkAuthor == "") != (forkEvent == "") {
		return mcpUnsigned{}, errors.New("a fork needs both fork_author and fork_event")
	}
	if forkAuthor != "" {
		tags = append(tags, []string{"a", "30818:" + forkAuthor + ":" + d, "", "fork"}, []string{"e", forkEvent, "", "fork"})
	}
	return mcpUnsigned{Kind: kindWikiArticle, CreatedAt: time.Now().Unix(), Tags: tags, Content: content}, nil
}

func mcpBuildWikiMerge(call mcp.Call) (mcpUnsigned, error) {
	d, destination, source, base, content := wiki.Normalize(call.String("d")), call.String("destination"), call.String("source"), call.String("base"), call.String("content")
	if d == "" || destination == "" || source == "" || strings.TrimSpace(content) == "" {
		return mcpUnsigned{}, errors.New("d, destination, source and content are required")
	}
	tags := [][]string{{"a", "30818:" + destination + ":" + d}, {"p", destination}, {"e", source, "", "source"}}
	if base != "" {
		tags = append(tags, []string{"e", base})
	}
	return mcpUnsigned{Kind: kindWikiMerge, CreatedAt: time.Now().Unix(), Tags: tags, Content: content}, nil
}

// mcpBuildSite builds a site manifest for the caller's own site or a named
// site under its key. The template passes the same validation as the signed
// event so a bad path is reported before anything is signed.
func mcpBuildSite(call mcp.Call) (mcpUnsigned, error) {
	kind, tags := sites.KindSite, [][]string{}
	if label := strings.TrimSpace(call.String("label")); label != "" {
		site, ok := sites.ParseSite(label)
		if !ok || site.Kind == sites.KindSiteSnapshot {
			return mcpUnsigned{}, errors.New("label must be your npub or a named site label under your key")
		}
		if site.PubKey != call.Actor {
			return mcpUnsigned{}, errors.New("label " + label + " is not a site under your key")
		}
		kind = site.Kind
		if site.Kind == sites.KindNamedSite {
			tags = append(tags, []string{"d", site.D})
		}
	}
	pairs, _ := call.Arguments["paths"].([]any)
	if len(pairs) == 0 {
		return mcpUnsigned{}, errors.New("paths is required: one [path, sha256] pair per file")
	}
	for _, pair := range pairs {
		values, _ := pair.([]any)
		if len(values) != 2 {
			return mcpUnsigned{}, errors.New("each entry of paths is a [path, sha256] pair")
		}
		path, _ := values[0].(string)
		hash, _ := values[1].(string)
		tags = append(tags, []string{"path", path, strings.ToLower(strings.TrimSpace(hash))})
	}
	if expiration := call.Int("expiration"); expiration > 0 {
		if int64(expiration) <= time.Now().Unix() {
			return mcpUnsigned{}, errors.New("expiration must be in the future")
		}
		tags = append(tags, []string{"expiration", strconv.Itoa(expiration)})
	}
	if err := sites.ValidateManifest(event.Event{Kind: kind, PubKey: call.Actor, Tags: tags}); err != nil {
		return mcpUnsigned{}, errors.New(strings.TrimPrefix(err.Error(), "invalid: "))
	}
	return mcpUnsigned{Kind: kind, CreatedAt: time.Now().Unix(), Tags: tags, Content: ""}, nil
}

func mcpCheckSite(e event.Event) error {
	if e.Kind != sites.KindSite && e.Kind != sites.KindNamedSite {
		return fmt.Errorf("kind %d is not a site manifest", e.Kind)
	}
	if err := sites.ValidateManifest(e); err != nil {
		return errors.New(strings.TrimPrefix(err.Error(), "invalid: "))
	}
	return nil
}

func mcpBuildRoom(call mcp.Call) (mcpUnsigned, error) {
	tags, err := mcpRoomTag(call)
	if err != nil {
		return mcpUnsigned{}, err
	}
	name := strings.TrimSpace(call.String("name"))
	if name == "" {
		return mcpUnsigned{}, errors.New("name is required")
	}
	tags = append(tags, []string{"name", name})
	if about := strings.TrimSpace(call.String("about")); about != "" {
		tags = append(tags, []string{"about", about})
	}
	if visibility := call.String("visibility"); visibility != "" {
		tags = append(tags, []string{"visibility", visibility})
	}
	return mcpUnsigned{Kind: event.KIND_CREATE_GROUP, CreatedAt: time.Now().Unix(), Tags: tags, Content: ""}, nil
}

func mcpBuildRequest(call mcp.Call) (mcpUnsigned, error) {
	pubkey, request, content := call.String("pubkey"), call.String("request"), call.String("content")
	if pubkey == "" || !contains(mcpRequestKinds, request) || strings.TrimSpace(content) == "" {
		return mcpUnsigned{}, errors.New("pubkey, request and content are required")
	}
	room, root, rootPubKey, rootKind := call.String("room"), call.String("root"), call.String("root_pubkey"), call.Int("root_kind")
	var kind int
	var tags [][]string
	switch {
	case room != "" && root != "":
		kind, tags = event.KIND_CHAT, [][]string{{"h", room}, {"e", root, "", "root"}}
	case room != "":
		kind, tags = event.KIND_CHAT, [][]string{{"h", room}}
	case root != "":
		if rootPubKey == "" || rootKind <= 0 {
			return mcpUnsigned{}, errors.New("root needs root_kind and root_pubkey")
		}
		k := strconv.Itoa(rootKind)
		kind, tags = 1111, [][]string{{"E", root, "", rootPubKey}, {"K", k}, {"P", rootPubKey}, {"e", root, "", rootPubKey}, {"k", k}}
		if rootPubKey != pubkey {
			tags = append(tags, []string{"p", rootPubKey})
		}
	default:
		return mcpUnsigned{}, errors.New("room or root is required")
	}
	tags = append(tags, []string{"request", request}, []string{"p", pubkey})
	if expiration := call.Int("expiration"); expiration > 0 {
		if int64(expiration) <= time.Now().Unix() {
			return mcpUnsigned{}, errors.New("expiration must be in the future")
		}
		tags = append(tags, []string{"expiration", strconv.Itoa(expiration)})
	}
	if subject := strings.TrimSpace(call.String("subject")); subject != "" {
		tags = append(tags, []string{"subject", subject})
	}
	return mcpUnsigned{Kind: kind, CreatedAt: time.Now().Unix(), Tags: tags, Content: content}, nil
}

func mcpCheckRoomEvent(e event.Event, kind int, what string) error {
	if e.Kind != kind {
		return fmt.Errorf("kind %d is not a %s", e.Kind, what)
	}
	if !community.ValidRoomID(event.Tag(e, "h")) {
		return errors.New("the h tag must name the room")
	}
	return nil
}

func mcpCheckMessage(e event.Event) error {
	if err := mcpCheckRoomEvent(e, event.KIND_CHAT, "chat message"); err != nil {
		return err
	}
	if strings.TrimSpace(e.Content) == "" {
		return errors.New("content must not be empty")
	}
	return mcpCheckAttachments(e)
}

func mcpCheckThread(e event.Event) error {
	if err := mcpCheckRoomEvent(e, event.KIND_THREAD, "thread"); err != nil {
		return err
	}
	if strings.TrimSpace(e.Content) == "" {
		return errors.New("content must not be empty")
	}
	return mcpCheckAttachments(e)
}

func mcpCheckReply(e event.Event) error {
	if err := mcpCheckRoomEvent(e, event.KIND_THREAD_REPLY, "thread reply"); err != nil {
		return err
	}
	if !hex64(event.Tag(e, "e")) {
		return errors.New("the e tag must name the thread root")
	}
	if strings.TrimSpace(e.Content) == "" {
		return errors.New("content must not be empty")
	}
	return mcpCheckAttachments(e)
}

func mcpCheckReact(e event.Event) error {
	if e.Kind != 7 {
		return fmt.Errorf("kind %d is not a reaction", e.Kind)
	}
	if !hex64(event.Tag(e, "e")) || !hex64(event.Tag(e, "p")) {
		return errors.New("the e and p tags must name the event and its author")
	}
	if room := event.Tag(e, "h"); room != "" && !community.ValidRoomID(room) {
		return errors.New("the h tag must name a room")
	}
	if !mcpReaction(e.Content) {
		return errors.New("content must be +, - or one emoji")
	}
	return nil
}

func mcpCheckWikiPage(e event.Event) error {
	if e.Kind != kindWikiArticle {
		return fmt.Errorf("kind %d is not a wiki page", e.Kind)
	}
	d := event.Tag(e, "d")
	if d == "" || d != wiki.Normalize(d) {
		return errors.New("the d tag must carry the normalized page name")
	}
	if strings.TrimSpace(event.Tag(e, "title")) == "" || strings.TrimSpace(e.Content) == "" {
		return errors.New("the title tag and content must not be empty")
	}
	for _, tag := range e.Tags {
		if len(tag) < 4 || (tag[3] != "fork" && tag[3] != "defer") {
			continue
		}
		if (tag[0] == "a" && !mcpWikiCoordinate.MatchString(tag[1])) || (tag[0] == "e" && !hex64(tag[1])) {
			return errors.New("fork and defer tags must name a 30818:<author>:<page> coordinate or a version id")
		}
	}
	return nil
}

func mcpCheckWikiMerge(e event.Event) error {
	if e.Kind != kindWikiMerge {
		return fmt.Errorf("kind %d is not a merge request", e.Kind)
	}
	coordinate := event.Tag(e, "a")
	if !mcpWikiCoordinate.MatchString(coordinate) {
		return errors.New("the a tag must name the target as 30818:<destination>:<page>")
	}
	if destination := event.Tag(e, "p"); !hex64(destination) || destination != strings.Split(coordinate, ":")[1] {
		return errors.New("the p tag must name the destination author of the a tag")
	}
	source := ""
	for _, tag := range e.Tags {
		if len(tag) >= 4 && tag[0] == "e" && tag[3] == "source" {
			source = tag[1]
		}
	}
	if !hex64(source) {
		return errors.New("an e tag with the source marker must name the proposed version")
	}
	if strings.TrimSpace(e.Content) == "" {
		return errors.New("content must not be empty")
	}
	return nil
}

func mcpCheckRoom(e event.Event) error {
	if err := mcpCheckRoomEvent(e, event.KIND_CREATE_GROUP, "room creation"); err != nil {
		return err
	}
	if strings.TrimSpace(event.Tag(e, "name")) == "" {
		return errors.New("the name tag must not be empty")
	}
	if visibility := event.Tag(e, "visibility"); visibility != "" && visibility != community.RoomOpen && visibility != community.RoomMembers {
		return errors.New("the visibility tag must be open or members")
	}
	return nil
}

func mcpCheckRequest(e event.Event) error {
	switch e.Kind {
	case event.KIND_CHAT:
		if err := mcpCheckRoomEvent(e, event.KIND_CHAT, "chat message"); err != nil {
			return err
		}
	case 1111:
		if !hex64(event.Tag(e, "E")) || !hex64(event.Tag(e, "P")) || event.Tag(e, "K") == "" || !hex64(event.Tag(e, "e")) || event.Tag(e, "k") == "" {
			return errors.New("the E, K, P, e and k tags must name the root event, its kind and its author")
		}
	default:
		return fmt.Errorf("kind %d is not a chat message or comment", e.Kind)
	}
	if !contains(mcpRequestKinds, event.Tag(e, "request")) {
		return errors.New("the request tag must be approve, decide or question")
	}
	asked := event.TagValues(e, "p")
	if len(asked) == 0 || !hex64(asked[len(asked)-1]) {
		return errors.New("a p tag must name the person asked")
	}
	if expiration := event.Tag(e, "expiration"); expiration != "" {
		if at, err := strconv.ParseInt(expiration, 10, 64); err != nil || at <= time.Now().Unix() {
			return errors.New("the expiration tag must be a Unix time in the future")
		}
	}
	if strings.TrimSpace(e.Content) == "" {
		return errors.New("content must not be empty")
	}
	return nil
}

// Long task builders. A request names its room and the keys it asks;
// every answer and cancel names the request, the requester and the room.

// mcpJobAnswerSchema is the plain-field schema shared by the answer tools.
// A result also takes artifacts.
func mcpJobAnswerSchema(artifacts bool) map[string]any {
	fields := map[string]any{"event": mcpEvent, "request": mcpJobEvent, "e": mcpHash, "p": mcpPubKey, "room": mcpRoomID, "content": mcpText}
	if artifacts {
		fields["artifacts"] = mcpArtifacts
	}
	return mcp.Object(fields)
}

func mcpBuildJobRequest(call mcp.Call) (mcpUnsigned, error) {
	room := strings.TrimSpace(call.String("room"))
	if !community.ValidRoomID(room) {
		return mcpUnsigned{}, errors.New("room is required and must be a room id")
	}
	tags := [][]string{{"h", room}}
	assignees, _ := call.Arguments["assignees"].([]any)
	for _, raw := range assignees {
		key, _ := raw.(string)
		if !hex64(key) {
			return mcpUnsigned{}, errors.New("assignees must be hex public keys")
		}
		tags = append(tags, []string{"p", key})
	}
	if len(assignees) == 0 {
		return mcpUnsigned{}, errors.New("assignees must name at least one key to ask")
	}
	subject, content := strings.TrimSpace(call.String("subject")), call.String("content")
	if subject == "" && strings.TrimSpace(content) == "" {
		return mcpUnsigned{}, errors.New("content or subject is required")
	}
	if subject != "" {
		tags = append(tags, []string{"subject", subject})
	}
	if root := call.String("root"); root != "" {
		if !hex64(root) {
			return mcpUnsigned{}, errors.New("root must be an event id")
		}
		tags = append(tags, []string{"e", root, "", "root"})
	}
	if expiration := call.Int("expiration"); expiration > 0 {
		if int64(expiration) <= time.Now().Unix() {
			return mcpUnsigned{}, errors.New("expiration must be in the future")
		}
		tags = append(tags, []string{"expiration", strconv.Itoa(expiration)})
	}
	return mcpUnsigned{Kind: event.KIND_JOB_REQUEST, CreatedAt: time.Now().Unix(), Tags: tags, Content: content}, nil
}

// mcpJobRequestArgument reads the request event an answer tool was given.
func mcpJobRequestArgument(call mcp.Call) (event.Event, bool, error) {
	raw, ok := call.Arguments["request"].(map[string]any)
	if !ok {
		return event.Event{}, false, nil
	}
	var request event.Event
	encoded, err := json.Marshal(raw)
	if err == nil {
		err = json.Unmarshal(encoded, &request)
	}
	if err != nil || !event.IsJobRequest(request.Kind) || !hex64(request.ID) || !hex64(request.PubKey) {
		return event.Event{}, false, errors.New("request must be a kind 43001 job request event with id and pubkey")
	}
	return request, true, nil
}

// mcpJobTarget resolves the request an answer names, from the request
// event or from e, p and room, and checks that the two agree.
func mcpJobTarget(call mcp.Call) (id, requester, room string, err error) {
	id, requester, room = call.String("e"), call.String("p"), strings.TrimSpace(call.String("room"))
	request, ok, err := mcpJobRequestArgument(call)
	if err != nil {
		return "", "", "", err
	}
	if ok {
		if id == "" {
			id = request.ID
		}
		if requester == "" {
			requester = request.PubKey
		}
		if room == "" {
			room = event.Tag(request, "h")
		}
		if id != request.ID || requester != request.PubKey || room != event.Tag(request, "h") {
			return "", "", "", errors.New("e, p and room must match the request: its id, its author and its h tag")
		}
	}
	if !hex64(id) || !hex64(requester) || !community.ValidRoomID(room) {
		return "", "", "", errors.New("request, or e (the request id), p (the requester) and room, are required")
	}
	return id, requester, room, nil
}

// mcpBuildJobAnswer builds an accepted, progress, result or error event.
// Progress, results and errors need content; a result may add artifacts.
func mcpBuildJobAnswer(kind int) func(mcp.Call) (mcpUnsigned, error) {
	return func(call mcp.Call) (mcpUnsigned, error) {
		id, requester, room, err := mcpJobTarget(call)
		if err != nil {
			return mcpUnsigned{}, err
		}
		content := call.String("content")
		if kind != event.KIND_JOB_ACCEPTED && strings.TrimSpace(content) == "" {
			return mcpUnsigned{}, errors.New("content is required")
		}
		tags := [][]string{{"h", room}, {"e", id}, {"p", requester}}
		if artifacts, ok := call.Arguments["artifacts"].([]any); ok && kind == event.KIND_JOB_RESULT {
			for _, raw := range artifacts {
				artifact, _ := raw.(map[string]any)
				name, _ := artifact["type"].(string)
				value, _ := artifact["value"].(string)
				if !mcpJobArtifactOK(name, strings.TrimSpace(value)) {
					return mcpUnsigned{}, errors.New("each artifact needs a type of e (an event id), a (a kind:pubkey:identifier coordinate) or r (an http or https URL) and a matching value")
				}
				tags = append(tags, []string{name, strings.TrimSpace(value)})
			}
		}
		return mcpUnsigned{Kind: kind, CreatedAt: time.Now().Unix(), Tags: tags, Content: content}, nil
	}
}

// mcpJobArtifactOK reports whether one artifact reference is well formed.
func mcpJobArtifactOK(name, value string) bool {
	switch name {
	case "e":
		return hex64(value)
	case "a":
		parts := strings.SplitN(value, ":", 3)
		if len(parts) != 3 || !hex64(parts[1]) {
			return false
		}
		_, err := strconv.Atoi(parts[0])
		return err == nil
	case "r":
		return strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://")
	}
	return false
}

func mcpBuildJobCancel(call mcp.Call) (mcpUnsigned, error) {
	id, room := call.String("e"), strings.TrimSpace(call.String("room"))
	request, ok, err := mcpJobRequestArgument(call)
	if err != nil {
		return mcpUnsigned{}, err
	}
	if ok {
		if id == "" {
			id = request.ID
		}
		if room == "" {
			room = event.Tag(request, "h")
		}
		if id != request.ID || room != event.Tag(request, "h") {
			return mcpUnsigned{}, errors.New("e and room must match the request: its id and its h tag")
		}
	}
	if !hex64(id) || !community.ValidRoomID(room) {
		return mcpUnsigned{}, errors.New("request, or e (the request id) and room, are required")
	}
	tags := [][]string{{"h", room}, {"e", id}}
	if assignee := call.String("p"); assignee != "" {
		if !hex64(assignee) {
			return mcpUnsigned{}, errors.New("p must be a hex public key")
		}
		tags = append(tags, []string{"p", assignee})
	}
	return mcpUnsigned{Kind: event.KIND_JOB_CANCEL, CreatedAt: time.Now().Unix(), Tags: tags, Content: call.String("content")}, nil
}

// mcpJobShape reports the gate's shape check without its NIP-01 prefix, so
// the tool's message reads as one sentence.
func mcpJobShape(e event.Event) error {
	if err := gates.JobShape(e); err != nil {
		return errors.New(strings.TrimPrefix(err.Error(), "invalid: "))
	}
	return nil
}

func mcpCheckJobRequest(e event.Event) error {
	if !event.IsJobRequest(e.Kind) {
		return fmt.Errorf("kind %d is not a job request", e.Kind)
	}
	if expiration := event.Tag(e, "expiration"); expiration != "" {
		if at, err := strconv.ParseInt(expiration, 10, 64); err != nil || at <= time.Now().Unix() {
			return errors.New("the expiration tag must be a Unix time in the future")
		}
	}
	if strings.TrimSpace(e.Content) == "" && strings.TrimSpace(event.Tag(e, "subject")) == "" {
		return errors.New("content or a subject tag is required")
	}
	return mcpJobShape(e)
}

func mcpCheckJobAnswer(kind int) func(event.Event) error {
	return func(e event.Event) error {
		if e.Kind != kind {
			return fmt.Errorf("kind %d is not a %s", e.Kind, gates.JobKindName(kind))
		}
		if kind != event.KIND_JOB_ACCEPTED && strings.TrimSpace(e.Content) == "" {
			return errors.New("content must not be empty")
		}
		if kind == event.KIND_JOB_RESULT {
			first := true
			for _, tag := range e.Tags {
				if len(tag) < 2 {
					continue
				}
				if tag[0] == "e" && first {
					first = false
					continue
				}
				if (tag[0] == "e" || tag[0] == "a" || tag[0] == "r") && !mcpJobArtifactOK(tag[0], tag[1]) {
					return fmt.Errorf("the %s artifact tag %q is not an event id, a coordinate or an http or https URL", tag[0], tag[1])
				}
			}
		}
		return mcpJobShape(e)
	}
}

func mcpCheckJobCancel(e event.Event) error {
	if !event.IsJobCancel(e.Kind) {
		return fmt.Errorf("kind %d is not a job cancel", e.Kind)
	}
	return mcpJobShape(e)
}

// mcpReaction accepts NIP-25 content: +, - or one emoji, which may be a
// sequence of a few non-ASCII code points such as a flag or a skin tone.
func mcpReaction(content string) bool {
	if content == "+" || content == "-" {
		return true
	}
	runes := []rune(content)
	if len(runes) == 0 || len(runes) > 10 {
		return false
	}
	for _, r := range runes {
		if r < 0x80 || unicode.IsSpace(r) || unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

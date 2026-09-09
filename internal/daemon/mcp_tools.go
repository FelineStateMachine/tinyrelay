package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
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

// The MCP tool table mirrors the browser tools in internal/webui/webmcp.js.
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
	mcpJobKind   = map[string]any{"type": "integer", "minimum": event.KIND_JOB_REQUEST_MIN, "maximum": event.KIND_JOB_REQUEST_MAX, "description": "NIP-90 job request kind, 5000 to 5999."}
	mcpMsats     = map[string]any{"type": "integer", "minimum": 0, "description": "Amount in millisats."}
	mcpJobStatus = map[string]any{"type": "string", "enum": event.JobFeedbackStatuses}
	mcpJobInputs = map[string]any{"type": "array", "items": mcp.Object(map[string]any{"data": mcpText, "type": map[string]any{"type": "string", "enum": event.JobInputTypes}, "relay": mcpText, "marker": mcpText}, "data", "type"), "description": "Job inputs, each becoming an i tag."}
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
	mcpIssueShape       = `Expected a signed kind 1621 event with tags ["a","30617:<owner>:<repo>"], ["p","<owner>"], ["subject","<title>"] and optional ["t","<label>"] tags, with the Markdown description in content.`
	mcpPullShape        = `Expected a signed kind 1618 event with tags ["a","30617:<owner>:<repo>"], ["p","<owner>"], ["subject","<title>"], ["c","<40-hex commit>"], ["clone","<http or https URL>"], optional ["merge-base","<40-hex commit>"] and optional ["t","<label>"] tags, with the description in content.`
	mcpCommentShape     = `Expected a signed kind 1111 event with NIP-22 tags ["a","30617:<owner>:<repo>"], ["E","<root id>","","<root pubkey>"], ["K","<root kind>"], ["P","<root pubkey>"], ["e","<parent id>","","<parent pubkey>"], ["k","<parent kind>"], ["p","<parent pubkey>"], with the comment in content. The parent is the root for a top-level comment. A review comment under a pull request or patch may add ["file","<path>"] and ["line","<n>","old" or "new"] to name one diff line.`
	mcpStatusShape      = `Expected a signed event of kind 1630 (open), 1631 (resolved or merged), 1632 (closed) or 1633 (draft) with tags ["a","30617:<owner>:<repo>"], ["e","<root id>","","root"] and ["p","<root pubkey>"].`
	mcpMessageShape     = `Expected a signed kind 9 event with tag ["h","<room id>"] and optional ["p","<pubkey>"] mentions, with the message in content.`
	mcpThreadShape      = `Expected a signed kind 11 event with tag ["h","<room id>"] and optional ["subject","<title>"], with the opening post in content.`
	mcpReplyShape       = `Expected a signed kind 12 event with tags ["h","<room id>"], ["e","<thread root id>"] and optional ["p","<root author>"], with the reply in content.`
	mcpReactShape       = `Expected a signed kind 7 event with tags ["e","<event id>"], ["p","<event author>"] and, for a room message, ["h","<room id>"], with "+", "-" or one emoji in content.`
	mcpWikiShape        = `Expected a signed kind 30818 event with tags ["d","<normalized page name>"], ["title","<title>"], optional ["summary","<summary>"] and, for a fork, ["a","30818:<author>:<page name>","","fork"] and ["e","<version id>","","fork"], with the Djot article in content.`
	mcpMergeShape       = `Expected a signed kind 818 event with tags ["a","30818:<destination>:<page name>"], ["p","<destination>"], ["e","<proposed version id>","","source"] and optional ["e","<base version id>"], with the explanation in content.`
	mcpRoomShape        = `Expected a signed kind 9007 event with tags ["h","<new room id>"], ["name","<name>"], optional ["about","<description>"] and ["visibility","open" or "members"].`
	mcpRequestShape     = `Expected a signed kind 9 event with ["h","<room id>"], or a signed kind 1111 event with NIP-22 tags ["E","<root id>","","<root pubkey>"], ["K","<root kind>"], ["P","<root pubkey>"], ["e","<root id>","","<root pubkey>"] and ["k","<root kind>"], carrying ["request","approve", "decide" or "question"], ["p","<asked pubkey>"] and optional ["expiration","<unix time>"] and ["subject","<subject>"], with the question in content.`
	mcpJobRequestShape  = `Expected a signed event of kind 5000 to 5999 with optional tags ["i","<data>","url" or "event" or "job" or "text","<relay>","<marker>"], ["output","<mime type>"], ["param","<key>","<value>"], ["bid","<millisats>"], ["relays","wss://..."], ["p","<provider pubkey>"] and ["expiration","<unix time>"], with content empty or the encrypted inputs.`
	mcpJobFeedbackShape = `Expected a signed kind 7000 event with tags ["status","payment-required" or "processing" or "error" or "success" or "partial","<info>"], ["e","<request id>"], ["p","<requester pubkey>"] and optional ["amount","<millisats>","<bolt11>"], with content empty or a partial result.`
	mcpJobResultShape   = `Expected a signed event of the request kind plus 1000 (6000 to 6999) with tags ["e","<request id>"], ["p","<requester pubkey>"], optional ["request","<request event JSON>"], the request's ["i",...] tags and optional ["amount","<millisats>","<bolt11>"], with the output in content.`
	mcpSiteShape        = `Expected a signed kind 15128 event for your own site, or a signed kind 35128 event with ["d","<site name>"] for a named site under your key, with one ["path","/<file path>","<sha256 of the file>"] tag per file, the blobs already uploaded, and an optional ["expiration","<unix time>"] tag.`
	mcpSignNext         = "Sign this event with your Nostr key and call the tool again with the signed event as the event argument."
	mcpAnswerNote       = " The answer arrives as a kind 7 reaction from the asked key on the published event: + approves, - declines, and any other content is the person's reply. Read the room or thread, or query kind 7 events with #e set to the event id, to collect it."
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
	add("read_wiki_page", "Read a wiki page: its preferred version with content, every version, merge requests and redirects. Pass author to prefer that key's version or version to open one by event id.", mcp.Object(map[string]any{"d": mcpPageName, "author": mcpPubKey, "version": mcpHash}, "d"), mcpReads, t.mcpBrowse("browsewikipage"))
	add("read_merge_request", "Read a wiki merge request with its status, the proposed version and the target version.", mcp.Object(map[string]any{"id": mcpHash}, "id"), mcpReads, t.mcpBrowse("browsewikimerge"))
	add("list_agents", "List granted agents with their name, key, owner, expiration, paused and revoked state, scope and last event. Requires an owner or moderator key.", mcp.Object(nil), mcpReads, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "listagents")
	})
	add("list_jobs", "List long task requests (NIP-90 job requests) visible to your key, each with its newest feedback status and its result when one exists. state narrows the list to open, done or all; mine lists only your own requests.", mcp.Object(map[string]any{"cursor": mcpText, "limit": mcpLimit, "state": map[string]any{"type": "string", "enum": jobStates}, "mine": map[string]any{"type": "boolean"}}), mcpReads, t.mcpBrowse("browsejobs"))
	add("read_job", "Read one long task request by event id with its feedback timeline and results.", mcp.Object(map[string]any{"id": mcpHash}, "id"), mcpReads, t.mcpBrowse("browsejob"))
	add("list_callbacks", "List event callbacks: id, owner, host, filter, paused state, failures and last delivery. Members and agents see their own; the owner and moderators see every callback. The secret is never listed.", mcp.Object(nil), mcpReads, func(ctx context.Context, call mcp.Call) (mcp.Result, error) {
		return t.mcpManage(ctx, call, "listcallbacks")
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
	add("post_message", "Post a kind 9 chat message in a room. Pass room and content (plus mentions, a list of public keys) to receive the unsigned event, sign it, then call again with the signed event. Posting in an open room joins it.", mcp.Object(map[string]any{"event": mcpEvent, "room": mcpRoomID, "content": mcpText, "mentions": map[string]any{"type": "array", "items": mcpPubKey}}), mcpPublishes, t.mcpWrite(mcpBuildMessage, mcpCheckMessage, mcpMessageShape))
	add("start_thread", "Start a kind 11 thread in a room. Pass room and content (plus an optional title) to receive the unsigned event, sign it, then call again with the signed event.", mcp.Object(map[string]any{"event": mcpEvent, "room": mcpRoomID, "title": mcpText, "content": mcpText}), mcpPublishes, t.mcpWrite(mcpBuildThread, mcpCheckThread, mcpThreadShape))
	add("reply_in_thread", "Reply to a thread in a room with a kind 12 event. Pass room, root (the thread's event id), root_pubkey and content to receive the unsigned event, sign it, then call again with the signed event.", mcp.Object(map[string]any{"event": mcpEvent, "room": mcpRoomID, "root": mcpHash, "root_pubkey": mcpPubKey, "content": mcpText}), mcpPublishes, t.mcpWrite(mcpBuildReply, mcpCheckReply, mcpReplyShape))
	add("react", "React to an event with a kind 7 reaction: + to like or approve, - to dislike or decline, or one emoji. Pass target, target_pubkey and content (plus room for a room message) to receive the unsigned event, sign it, then call again with the signed event. A + or - from a wiki merge request's destination author answers the request.", mcp.Object(map[string]any{"event": mcpEvent, "target": mcpHash, "target_pubkey": mcpPubKey, "content": map[string]any{"type": "string", "minLength": 1, "description": "+, - or one emoji."}, "room": mcpRoomID}), mcpPublishes, t.mcpWrite(mcpBuildReact, mcpCheckReact, mcpReactShape))
	add("publish_wiki_page", "Publish or replace your version of a kind 30818 wiki page in Djot markup. Pass d (the page name), title and content, plus optional summary and, to fork another author's version, fork_author and fork_event, to receive the unsigned event, sign it, then call again with the signed event.", mcp.Object(map[string]any{"event": mcpEvent, "d": mcpPageName, "title": mcpText, "summary": mcpText, "content": mcpText, "fork_author": mcpPubKey, "fork_event": mcpHash}), mcpPublishes, t.mcpWrite(mcpBuildWikiPage, mcpCheckWikiPage, mcpWikiShape))
	add("propose_wiki_merge", "Ask a wiki author to take in changes from another version with a kind 818 merge request. Pass d, destination (the author asked), source (the proposed version's event id) and content, plus an optional base version id, to receive the unsigned event, sign it, then call again with the signed event. The destination author answers with a + or - reaction.", mcp.Object(map[string]any{"event": mcpEvent, "d": mcpPageName, "destination": mcpPubKey, "source": mcpHash, "base": mcpHash, "content": mcpText}), mcpPublishes, t.mcpWrite(mcpBuildWikiMerge, mcpCheckWikiMerge, mcpMergeShape))
	add("publish_site", "Publish a static site manifest (NIP-5A). Upload the files to the blob store first, then pass paths as [path, sha256] pairs, an optional label (your npub for your own site, the default, or a named site label under your key) and an optional expiration, to receive the unsigned kind 15128 or 35128 event, sign it, then call again with the signed event. An agent needs a sites grant that covers the label; a grant with a ttl requires the expiration.", mcp.Object(map[string]any{"event": mcpEvent, "label": map[string]any{"type": "string", "minLength": 1, "description": "Site label: your npub, or a named site label under your key. Defaults to your own site."}, "paths": map[string]any{"type": "array", "items": map[string]any{"type": "array", "items": mcpText}, "description": "One [path, sha256] pair per file, such as [\"/index.html\", \"<sha256>\"]."}, "expiration": mcpUnixTime}), mcpPublishes, t.mcpWrite(mcpBuildSite, mcpCheckSite, mcpSiteShape))
	add("create_room", "Create a chat room with a kind 9007 event. Relay members may do this. Pass room (the new id), name and optional about and visibility (open or members) to receive the unsigned event, sign it, then call again with the signed event. The signer becomes the room owner.", mcp.Object(map[string]any{"event": mcpEvent, "room": mcpRoomID, "name": mcpText, "about": mcpText, "visibility": map[string]any{"type": "string", "enum": []string{"open", "members"}}}), mcpPublishes, t.mcpWrite(mcpBuildRoom, mcpCheckRoom, mcpRoomShape))
	add("request_decision", "Ask a person for an approval, a decision or an answer. Pass pubkey (the person asked), request (approve, decide or question) and content, plus room for a kind 9 room message or root, root_kind and root_pubkey for a kind 1111 comment under an issue, pull request or other event, and optional expiration and subject, to receive the unsigned event carrying a request tag, sign it, then call again with the signed event."+mcpAnswerNote, mcp.Object(map[string]any{"event": mcpEvent, "pubkey": mcpPubKey, "request": map[string]any{"type": "string", "enum": mcpRequestKinds}, "content": mcpText, "room": mcpRoomID, "root": mcpHash, "root_kind": map[string]any{"type": "integer", "minimum": 0}, "root_pubkey": mcpPubKey, "expiration": mcpUnixTime, "subject": mcpText}), mcpPublishes, t.mcpWrite(mcpBuildRequest, mcpCheckRequest, mcpRequestShape))
	add("request_job", "Ask for a long task with a NIP-90 job request. Pass kind (5000 to 5999) and inputs, plus optional output, params, bid in millisats, relays and expiration, to receive the unsigned event, sign it, then call again with the signed event. A serving agent answers with job feedback and a result naming the request; read them with read_job.", mcp.Object(map[string]any{"event": mcpEvent, "kind": mcpJobKind, "inputs": mcpJobInputs, "output": map[string]any{"type": "string", "description": "Expected output MIME type."}, "params": map[string]any{"type": "object", "description": "Job parameters as key and string value, each becoming a param tag."}, "bid": mcpMsats, "relays": map[string]any{"type": "array", "items": mcpText}, "expiration": mcpUnixTime}), mcpPublishes, t.mcpWrite(mcpBuildJobRequest, mcpCheckJobRequest, mcpJobRequestShape))
	add("job_feedback", "Report progress on a long task with a kind 7000 job feedback event. Pass e (the request id), p (the requester) and status (payment-required, processing, error, success or partial), plus optional info, amount in millisats, invoice and content, to receive the unsigned event, sign it, then call again with the signed event.", mcp.Object(map[string]any{"event": mcpEvent, "e": mcpHash, "p": mcpPubKey, "status": mcpJobStatus, "info": mcpText, "amount": mcpMsats, "invoice": mcpText, "content": mcpText}), mcpPublishes, t.mcpWrite(mcpBuildJobFeedback, mcpCheckJobFeedback, mcpJobFeedbackShape))
	add("job_result", "Deliver a long task's output with a NIP-90 job result. Pass request (the job request event) or kind, e and p, plus content and optional amount and invoice, to receive the unsigned event of the request kind plus 1000, sign it, then call again with the signed event.", mcp.Object(map[string]any{"event": mcpEvent, "request": map[string]any{"type": "object", "description": "The job request event being answered."}, "kind": map[string]any{"type": "integer", "minimum": event.KIND_JOB_RESULT_MIN, "maximum": event.KIND_JOB_RESULT_MAX}, "e": mcpHash, "p": mcpPubKey, "content": mcpText, "amount": mcpMsats, "invoice": mcpText}), mcpPublishes, t.mcpWrite(mcpBuildJobResult, mcpCheckJobResult, mcpJobResultShape))
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
		return mcp.Value(map[string]any{"unsigned": unsigned, "next": mcpSignNext}), nil
	}
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
	content := call.String("content")
	if strings.TrimSpace(content) == "" {
		return mcpUnsigned{}, errors.New("content is required")
	}
	return mcpUnsigned{Kind: event.KIND_CHAT, CreatedAt: time.Now().Unix(), Tags: mcpPubKeys(call, "mentions", tags), Content: content}, nil
}

func mcpBuildThread(call mcp.Call) (mcpUnsigned, error) {
	tags, err := mcpRoomTag(call)
	if err != nil {
		return mcpUnsigned{}, err
	}
	title, content := strings.TrimSpace(call.String("title")), call.String("content")
	if strings.TrimSpace(content) == "" {
		return mcpUnsigned{}, errors.New("content is required")
	}
	if title != "" {
		tags = append(tags, []string{"subject", title})
	}
	return mcpUnsigned{Kind: event.KIND_THREAD, CreatedAt: time.Now().Unix(), Tags: tags, Content: content}, nil
}

func mcpBuildReply(call mcp.Call) (mcpUnsigned, error) {
	tags, err := mcpRoomTag(call)
	if err != nil {
		return mcpUnsigned{}, err
	}
	root, rootPubKey, content := call.String("root"), call.String("root_pubkey"), call.String("content")
	if root == "" || strings.TrimSpace(content) == "" {
		return mcpUnsigned{}, errors.New("root and content are required")
	}
	tags = append(tags, []string{"e", root})
	if rootPubKey != "" {
		tags = append(tags, []string{"p", rootPubKey})
	}
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
		return mcpUnsigned{}, errors.New("pass either room or root, not both")
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
	return nil
}

func mcpCheckThread(e event.Event) error {
	if err := mcpCheckRoomEvent(e, event.KIND_THREAD, "thread"); err != nil {
		return err
	}
	if strings.TrimSpace(e.Content) == "" {
		return errors.New("content must not be empty")
	}
	return nil
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
	return nil
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

// Long task builders. A request carries its inputs and terms as NIP-90
// tags; feedback and results name the request and the requester.

func mcpBuildJobRequest(call mcp.Call) (mcpUnsigned, error) {
	kind := call.Int("kind")
	if !event.IsJobRequest(kind) {
		return mcpUnsigned{}, errors.New("kind is required and must be 5000 to 5999")
	}
	tags := [][]string{}
	if inputs, ok := call.Arguments["inputs"].([]any); ok {
		for _, raw := range inputs {
			input, _ := raw.(map[string]any)
			data, _ := input["data"].(string)
			kind, _ := input["type"].(string)
			relayHint, _ := input["relay"].(string)
			marker, _ := input["marker"].(string)
			if data == "" || !event.IsJobInputType(kind) {
				return mcpUnsigned{}, errors.New("each input needs data and a type of url, event, job or text")
			}
			if (kind == "event" || kind == "job") && !hex64(data) {
				return mcpUnsigned{}, errors.New("an input of type event or job must name an event id")
			}
			tag := []string{"i", data, kind}
			if relayHint != "" || marker != "" {
				tag = append(tag, relayHint)
			}
			if marker != "" {
				tag = append(tag, marker)
			}
			tags = append(tags, tag)
		}
	}
	if output := strings.TrimSpace(call.String("output")); output != "" {
		tags = append(tags, []string{"output", output})
	}
	if params, ok := call.Arguments["params"].(map[string]any); ok {
		keys := make([]string, 0, len(params))
		for key := range params {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value, ok := params[key].(string)
			if key == "" || !ok {
				return mcpUnsigned{}, errors.New("params must map names to string values")
			}
			tags = append(tags, []string{"param", key, value})
		}
	}
	if _, present := call.Arguments["bid"]; present {
		bid := call.Int("bid")
		if bid < 0 {
			return mcpUnsigned{}, errors.New("bid must be an amount in millisats")
		}
		tags = append(tags, []string{"bid", strconv.Itoa(bid)})
	}
	if relays, ok := call.Arguments["relays"].([]any); ok {
		tag := []string{"relays"}
		for _, value := range relays {
			if text, _ := value.(string); strings.TrimSpace(text) != "" {
				tag = append(tag, strings.TrimSpace(text))
			}
		}
		if len(tag) > 1 {
			tags = append(tags, tag)
		}
	}
	if expiration := call.Int("expiration"); expiration > 0 {
		if int64(expiration) <= time.Now().Unix() {
			return mcpUnsigned{}, errors.New("expiration must be in the future")
		}
		tags = append(tags, []string{"expiration", strconv.Itoa(expiration)})
	}
	return mcpUnsigned{Kind: kind, CreatedAt: time.Now().Unix(), Tags: tags, Content: ""}, nil
}

// mcpJobAmount appends the amount tag a provider asks to be paid, with its
// invoice when one is given.
func mcpJobAmount(call mcp.Call, tags [][]string) ([][]string, error) {
	if _, present := call.Arguments["amount"]; !present {
		return tags, nil
	}
	amount := call.Int("amount")
	if amount < 0 {
		return nil, errors.New("amount must be in millisats")
	}
	tag := []string{"amount", strconv.Itoa(amount)}
	if invoice := strings.TrimSpace(call.String("invoice")); invoice != "" {
		tag = append(tag, invoice)
	}
	return append(tags, tag), nil
}

func mcpBuildJobFeedback(call mcp.Call) (mcpUnsigned, error) {
	request, requester, status := call.String("e"), call.String("p"), call.String("status")
	if !hex64(request) || !hex64(requester) || !event.IsJobFeedbackStatus(status) {
		return mcpUnsigned{}, errors.New("e, p and status are required; status must be payment-required, processing, error, success or partial")
	}
	statusTag := []string{"status", status}
	if info := strings.TrimSpace(call.String("info")); info != "" {
		statusTag = append(statusTag, info)
	}
	tags, err := mcpJobAmount(call, [][]string{statusTag, {"e", request}, {"p", requester}})
	if err != nil {
		return mcpUnsigned{}, err
	}
	return mcpUnsigned{Kind: event.KIND_JOB_FEEDBACK, CreatedAt: time.Now().Unix(), Tags: tags, Content: call.String("content")}, nil
}

func mcpBuildJobResult(call mcp.Call) (mcpUnsigned, error) {
	var request event.Event
	hasRequest := false
	if raw, ok := call.Arguments["request"].(map[string]any); ok {
		encoded, err := json.Marshal(raw)
		if err == nil {
			err = json.Unmarshal(encoded, &request)
		}
		if err != nil || !event.IsJobRequest(request.Kind) || !hex64(request.ID) || !hex64(request.PubKey) {
			return mcpUnsigned{}, errors.New("request must be a job request event with id, pubkey and a kind of 5000 to 5999")
		}
		hasRequest = true
	}
	kind, id, requester, content := call.Int("kind"), call.String("e"), call.String("p"), call.String("content")
	if hasRequest {
		if kind == 0 {
			kind = event.JobResultKind(request.Kind)
		}
		if id == "" {
			id = request.ID
		}
		if requester == "" {
			requester = request.PubKey
		}
		if kind != event.JobResultKind(request.Kind) || id != request.ID || requester != request.PubKey {
			return mcpUnsigned{}, errors.New("kind, e and p must match the request: its kind plus 1000, its id and its author")
		}
	}
	if !event.IsJobResult(kind) || !hex64(id) || !hex64(requester) {
		return mcpUnsigned{}, errors.New("request, or kind (the request kind plus 1000), e and p, are required")
	}
	if strings.TrimSpace(content) == "" {
		return mcpUnsigned{}, errors.New("content is required")
	}
	tags := [][]string{}
	if hasRequest {
		canonical, err := event.Canonical(request)
		if err != nil {
			return mcpUnsigned{}, err
		}
		tags = append(tags, []string{"request", string(canonical)})
	}
	tags = append(tags, []string{"e", id})
	if hasRequest {
		for _, tag := range request.Tags {
			if len(tag) > 1 && tag[0] == "i" {
				tags = append(tags, append([]string(nil), tag...))
			}
		}
	}
	tags, err := mcpJobAmount(call, append(tags, []string{"p", requester}))
	if err != nil {
		return mcpUnsigned{}, err
	}
	return mcpUnsigned{Kind: kind, CreatedAt: time.Now().Unix(), Tags: tags, Content: content}, nil
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
	return mcpJobShape(e)
}

func mcpCheckJobFeedback(e event.Event) error {
	if e.Kind != event.KIND_JOB_FEEDBACK {
		return fmt.Errorf("kind %d is not job feedback", e.Kind)
	}
	return mcpJobShape(e)
}

func mcpCheckJobResult(e event.Event) error {
	if !event.IsJobResult(e.Kind) {
		return fmt.Errorf("kind %d is not a job result", e.Kind)
	}
	if request := event.Tag(e, "request"); request != "" && (!json.Valid([]byte(request)) || !strings.HasPrefix(strings.TrimSpace(request), "{")) {
		return errors.New("the request tag must carry the job request event as JSON")
	}
	if strings.TrimSpace(e.Content) == "" {
		return errors.New("content must not be empty")
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

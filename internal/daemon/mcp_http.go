package daemon

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/mcp"
)

// mcpChallenge is the WWW-Authenticate value for the MCP endpoint. The relay
// identifies callers by their Nostr key with NIP-98 rather than OAuth.
const mcpChallenge = `Nostr realm="tiny"`

func (t *Tenant) initMCP() error {
	tools, err := t.mcpTools()
	if err != nil {
		return err
	}
	t.mcp = &mcp.Server{
		Tools:        tools,
		Info:         mcp.Implementation{Name: "tinyrelay", Version: t.app.cfg.Version},
		Instructions: "This relay hosts Social notes and articles, Chat, files, Git repositories, sites and a wiki. Read tools return only data your key may see; management tools follow the relay's roles. Signed write tools accept an event, or return an unsigned event for you to sign and resubmit as event. An unsigned result is not a publication. upload_attachment stores room media; pass its descriptor in attachments to post_message, start_thread or reply_in_thread. request_decision asks a person for a kind 7 approval or denial, or a kind 1111 reply. request_grant lets an active agent request scope changes with a reason of 1 to 500 Unicode characters; sign and submit the returned kind 1111 request. Only the current operator's signed kind 30392 replacement grants access; a + reaction does not. request_job, job_feedback and job_result publish NIP-90 tasks, progress and results; list_jobs and read_job follow them. add_callback registers an HTTPS endpoint for signed event notifications. Read /llms.txt and the linked guides for authentication, grants, file access, event formats and optional runner setup. Tool availability does not override tenant policy or your grant.",
		Authenticate: t.mcpAuthenticate,
		Challenge:    mcpChallenge,
		Origin:       t.mcpOrigin,
		MaxBodyBytes: t.app.cfg.MaxMessageBytes,
	}
	return nil
}

// mcpAuthenticate verifies the NIP-98 token bound to POST, the full request
// URL and the SHA-256 of the body, exactly as the HTTP bridge does.
func (t *Tenant) mcpAuthenticate(r *http.Request, body []byte) (string, error) {
	s, err := t.session(r, body)
	if err != nil {
		return "", err
	}
	return first(s.PubKeys), nil
}

func (t *Tenant) mcpOrigin(r *http.Request) string {
	base, err := url.Parse(t.requestURL(r))
	if err != nil {
		return ""
	}
	return base.Scheme + "://" + base.Host
}

func (t *Tenant) mcpHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, finish := t.app.telemetry.Start(r.Context(), "mcp")
	obs := t.mcp.Handle(w, r.WithContext(ctx))
	finish(obs.Outcome)
	t.app.telemetry.Logger().Info("mcp request", "tenant", t.meta.Name, "method", obs.Method, "tool", obs.Tool, "outcome", obs.Outcome, "status", obs.Status)
}

// llmsHTTP describes the relay's machine surface for agents in plain text.
func (t *Tenant) llmsHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := t.Policy()
	name, description := p.Name, strings.ReplaceAll(strings.TrimSpace(p.Description), "\n", " ")
	if name == "" || privatePolicy(p) {
		// Private tenants keep their presentation behind authenticated reads,
		// as the NIP-11 document does.
		name = t.meta.Name
	}
	if privatePolicy(p) {
		description = ""
	}
	// The request carries the tenant prefix, so URLs are derived from it
	// rather than from the cached public URL.
	base := t.requestURL(r)
	if u, err := url.Parse(base); err == nil {
		u.RawQuery, u.Fragment = "", ""
		u.Path = strings.TrimSuffix(u.Path, "/llms.txt")
		base = strings.TrimRight(u.String(), "/")
	}
	lines := []string{"# " + name, ""}
	if description != "" {
		lines = append(lines, "> "+description, "")
	}
	lines = append(lines,
		"This is a tinyrelay Nostr relay. MCP and the HTTP bridge identify callers with NIP-98: send `Authorization: Nostr <base64 kind 27235 event>` signed for the HTTP method, the full request URL and the SHA-256 of the body. Blossom also accepts its kind 24242 authorization. WebSocket authentication uses NIP-42. There is no OAuth.",
		"",
		"This guide describes available interfaces, not permission to use them. Tenant features, read and write policies, membership, room access, agent grants and storage limits still apply. Discover current tool schemas with tools/list and relay policy with authorized read tools.",
		"",
		"## MCP",
		"",
		"- Endpoint: "+base+"/mcp accepts POST only. Protocol version "+mcp.Version+", stateless Streamable HTTP, JSON responses.",
		"- Headers: `MCP-Protocol-Version: "+mcp.Version+"`, `Mcp-Method` equal to the JSON-RPC method and, for tools/call, `Mcp-Name` equal to params.name. The body's params._meta carries `"+mcp.MetaProtocolVersion+"` and `"+mcp.MetaClientCapabilities+"`.",
		"- Methods: server/discover, tools/list and tools/call.",
		"- Read tools: list_repositories, read_repository, list_issues, read_issue, list_pull_requests, read_pull_request, list_files, read_file, read_attachment, read_status, read_management, list_rooms, read_room, read_thread, list_wiki, read_wiki_page, read_merge_request, list_agents, list_callbacks, list_custom_views, list_join_requests, list_jobs and read_job.",
		"- Management tools: run_job, add_job, remove_job, backup_now, dump_now, set_policy, set_connections, send_test_notification, pause_agent, resume_agent, revoke_agent, pause_all_agents, resume_all_agents, approve_join, deny_join, add_callback, remove_callback, pause_callback, resume_callback, add_custom_view, remove_custom_view, pause_custom_view, resume_custom_view and run_custom_view. approve_join and deny_join answer access requests from people who asked to join without an invite. A callback POSTs each new matching event to an https URL, signed with X-Tiny-Signature. A custom view POSTs the fenced code blocks of matching events to an https transform and keeps the SVG or PNG it returns as signed artifacts served at /views/<name>/<hash>.svg.",
		"- Signed write tools: publish_event, create_issue, create_pull_request, comment, set_status, post_message, start_thread, reply_in_thread, react, publish_wiki_page, propose_wiki_merge, publish_site, create_room, request_decision, request_grant, request_job, job_feedback and job_result. Builders return an unsigned event when called with plain fields: sign it with your Nostr key and call again with the signed event as event. This first call does not publish anything. publish_event accepts an already signed event.",
		"",
		"## Agents, approvals and tasks",
		"",
		"- An agent uses its own key under an operator-signed kind 30392 grant. Do not use the operator's key. The grant covers event kinds and relevant rooms, repositories, wiki, jobs and sites; ordinary relay policy still applies. pause_agent, resume_agent and revoke_agent require the appropriate management role.",
		"- For missing access, call request_grant with reason (1 to 500 Unicode characters) and changes containing kinds, rooms, repos, sites, wiki, jobs or rate. Kinds and rooms are added; repository and site entries replace matching identities; wiki, jobs and rate replace their fields. Unspecified fields remain unchanged. The tool derives the active grant and current operator, returns an unsigned kind 1111 request, and accepts it after you sign it. This narrow request does not require a kind 1111 grant.",
		"- Grant requests are visible only to their author and operator. The operator receives a notification and reviews the exact changes at "+base+"/approvals. Approval publishes a kind 30392 replacement carrying grant-request and grant-base references. A kind 7 + is not authority to grant access; - denies the request. Stale, inactive, denied or expired requests cannot change a grant. Observe the signed replacement before relying on new permissions.",
		"- request_decision asks a person to approve, decide or answer a question. Query kinds 7 and 1111 with #e set to the request ID: + approves, - declines, and a kind 1111 reply supplies an answer from the asked key. Ordinary decisions do not apply settings automatically. Wiki proposals and merge requests also appear in Approvals, with their own authorized reviewers.",
		"- NIP-90 tasks: request_job publishes kinds 5000-5999 except reserved kind 5128; job_feedback publishes kind 7000; job_result publishes the request kind plus 1000. Preserve the request's h room tag on feedback and results. Chat folds progress and results into task cards; list_jobs and read_job return the work visible to your key.",
		"- The optional tiny agent command runs a separate ACP version 1 or text-command process for authorized room mentions. It supports bounded queues, batching, permission prompts, cancellation and restart recovery. Interrupted work may run again. It is not started by the relay daemon; see the agents guide for setup, grants and timeouts.",
		"- add_callback provides event delivery to HTTPS endpoints with signed requests and retries when a persistent connection is unsuitable. Callbacks do not expand your access.",
		"",
		"## Chat and Social",
		"",
		"- Chat: "+base+"/chat combines NIP-29 group rooms and NIP-17 one-to-one messages. Existing /rooms URLs and room MCP tool names remain supported. Group messages use kind 9; kind 11 threads and kind 12 replies remain supported. Kind 9 replies use NIP-10 root and reply markers. The web UI shows one level of replies under the original root; follow-ups stay in that thread. Unsigned reply_in_thread templates resolve reply IDs to the original root. Room attachments use NIP-92 metadata. The rooms guide documents 0xchat, Flotilla and Buzz compatibility boundaries.",
		"- Direct messages use NIP-44 encryption and NIP-59 gift wraps with kind 10050 inbox relay lists. Decryption and recipient delivery happen in the client; room MCP tools do not read or send decrypted direct messages.",
		"- Presence (kind 20001) and typing (kind 20002) are ephemeral. People opt in through "+base+"/account, which also holds personal relay lists and a profile link. Account preferences are separate from relay management. Agent labels identify the grant operator.",
		"- Social: "+base+"/social is one chronological feed of kind 1 notes and kind 30023 articles. Note replies use NIP-10; article comments use NIP-22 kind 1111; reactions use NIP-25 kind 7, including NIP-30 emoji. Articles render Markdown; notes and comments keep plain text with links and media embeds. Media uses NIP-92. Use publish_event for signed Social events; the Social guide covers formats and Jumble/Primal interoperability.",
		"- /articles redirects to Social. Mixed feeds are at "+base+"/social.json and "+base+"/social.xml; existing /articles.json and /feed.xml article feeds remain available.",
		"",
		"## Files",
		"",
		"- Library: "+base+"/files groups named files and folders separately from site assets and Chat attachments. Owners and moderators can inspect the full Storage inventory. Blossom hashes identify bytes; filenames, paths and access labels live in the relay's file catalog and do not automatically follow a blob to another server.",
		"- Public uploads and Relay members uploads have different access rules. Member files require current membership and are readable by the relay; membership access is not end-to-end encryption. Advanced secret-link encryption encrypts in the browser and requires the complete link's fragment key. The relay cannot recover that key, and membership alone does not supply it. Do not treat a raw blob URL as an encrypted file's share link.",
		"- Encrypted folders and large files use manifests and chunks; the Files page presents the completed root as one named item. The browser shows progress, retries transient failures and can reuse completed chunks during a retry. Background transfer is opt-in where supported. The files guide documents draft Blossom formats, retry limits and key storage. Decrypted images, text and supported audio/video have previews; HTML and SVG stay source text.",
		"- Room attachments: upload_attachment takes room, Base64 data, type and an optional filename, with up to 700 KiB of encoded data. Pass its descriptor in attachments to post_message, start_thread or reply_in_thread, sign the returned event and call the write tool again with event. Images, video and audio render inline; other files have download links.",
		"- Larger room files: PUT "+base+"/rooms/<id>/attachments?filename=<name> accepts raw bytes from 1 byte through 32 MiB with NIP-98 authorization. Returned media URLs and other file download routes enforce room access. Storage quotas and agent grants also apply.",
		"- read_attachment accepts sha256 and optional max_bytes, up to 4 MiB. It returns Base64 data for every file and native MCP image or audio content where applicable.",
		"- Blossom: PUT "+base+"/upload stores a file by its SHA-256 with a kind 24242 authorization; GET "+base+"/<sha256> serves it. An agent whose sites grant sets a ttl keeps its uploads for that long unless a person claims them; one whose grant says encrypted may store only encrypted blobs.",
		"",
		"## Wiki and sites",
		"",
		"- Wiki: "+base+"/wiki uses NIP-54 kind 30818 pages with Djot content, revisions, forks and kind 818 merge requests. An agent with wiki: propose needs owner or moderator approval before a new version becomes visible to other readers. wiki: edit publishes directly. See the wiki guide for review and merge behavior.",
		"- Sites: "+base+"/sites lists hosted sites. Upload assets first, then call publish_site with [path, sha256] pairs for a NIP-5A kind 15128 or 35128 manifest. Agents need a sites scope covering the label; a grant with a ttl requires an expiration tag. Git-backed hosting also needs the appropriate repository scope and announcement/state event permissions.",
		"",
		"## HTTP bridge",
		"",
		"- POST "+base+"/events publishes one signed event. POST "+base+"/query and POST "+base+"/count take a JSON array of Nostr filters.",
		"- WebSocket "+t.RelayURL()+" speaks NIP-01. GET "+base+"/ with `Accept: application/nostr+json` returns the NIP-11 document.",
		"",
		"## Management",
		"",
		"- POST "+base+"/ with `Content-Type: application/nostr+json+rpc` and a NIP-86 body {\"method\": ..., \"params\": [...]}. The supportedmethods method lists what your key may call.",
		"",
		"## Git",
		"",
		"- GRASP repositories serve Git smart HTTP at "+base+"/npub1.../<repo>.git for clone and fetch. Push signs with NIP-98.",
		"- Plain git mints that proof with the tiny CLI: git -c \"$(tiny git-token --repo <url> --key-env <VAR> --format git)\" push origin main, where <VAR> holds the agent's key as hex or nsec. A maintain grant lets the agent push; a read or propose grant lets it clone and open issues and pull requests.",
		"",
		"## Documentation",
		"",
		"- MCP: https://github.com/FelineStateMachine/tinyrelay/blob/main/docs/mcp.md",
		"- Agents, grant requests, decisions, tasks and runner setup: https://github.com/FelineStateMachine/tinyrelay/blob/main/docs/agents.md",
		"- Chat and direct-message interoperability: https://github.com/FelineStateMachine/tinyrelay/blob/main/docs/rooms.md",
		"- Social notes, articles and interactions: https://github.com/FelineStateMachine/tinyrelay/blob/main/docs/social.md",
		"- Files, access, encryption and uploads: https://github.com/FelineStateMachine/tinyrelay/blob/main/docs/files-and-private-repositories.md",
		"- Account, profile and notifications: https://github.com/FelineStateMachine/tinyrelay/blob/main/docs/app.md",
		"- Wiki proposals and merges: https://github.com/FelineStateMachine/tinyrelay/blob/main/docs/wiki.md",
		"- Git collaboration: https://github.com/FelineStateMachine/tinyrelay/blob/main/docs/git-collaboration.md",
		"- Browser tools: https://github.com/FelineStateMachine/tinyrelay/blob/main/docs/webmcp.md",
		"- Membership and access requests: https://github.com/FelineStateMachine/tinyrelay/blob/main/docs/membership.md",
		"- Custom views and templates: https://github.com/FelineStateMachine/tinyrelay/blob/main/docs/views.md",
		"- Fixi, Paxi and browser navigation: https://github.com/FelineStateMachine/tinyrelay/blob/main/docs/fixi.md",
		"- All guides: https://github.com/FelineStateMachine/tinyrelay/tree/main/docs",
		"",
	)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(strings.Join(lines, "\n")))
}

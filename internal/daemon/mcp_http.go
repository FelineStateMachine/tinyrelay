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
		Instructions: "This relay hosts Nostr events, files, Git repositories, chat rooms and a wiki, and admits agents under signed grants. Read tools return the data your key may see; management tools, including the agent controls, follow the relay's roles. Write tools accept a signed Nostr event, or build the unsigned event for you to sign when called with plain fields. request_decision asks a person for an answer that arrives as a kind 7 reaction. request_job asks for a long task; a serving agent answers it with job_feedback and job_result, and list_jobs and read_job follow the work. add_callback registers an https URL that is woken with each new event matching a filter, so an agent that cannot hold a connection open still learns of new issues, pushes, room messages and requests.",
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
		"This is a tinyrelay Nostr relay. Every authenticated surface below identifies the caller with NIP-98: send `Authorization: Nostr <base64 kind 27235 event>` signed for the HTTP method, the full request URL and the SHA-256 of the body. There is no OAuth.",
		"",
		"## MCP",
		"",
		"- Endpoint: "+base+"/mcp accepts POST only. Protocol version "+mcp.Version+", stateless Streamable HTTP, JSON responses.",
		"- Headers: `MCP-Protocol-Version: "+mcp.Version+"`, `Mcp-Method` equal to the JSON-RPC method and, for tools/call, `Mcp-Name` equal to params.name. The body's params._meta carries `"+mcp.MetaProtocolVersion+"` and `"+mcp.MetaClientCapabilities+"`.",
		"- Methods: server/discover, tools/list and tools/call.",
		"- Read tools: list_repositories, read_repository, list_issues, read_issue, list_pull_requests, read_pull_request, list_files, read_file, read_status, read_management, list_rooms, read_room, read_thread, list_wiki, read_wiki_page, read_merge_request, list_agents, list_callbacks, list_custom_views, list_join_requests, list_jobs and read_job.",
		"- Management tools: run_job, add_job, remove_job, backup_now, dump_now, set_policy, set_connections, send_test_notification, pause_agent, resume_agent, revoke_agent, pause_all_agents, resume_all_agents, approve_join, deny_join, add_callback, remove_callback, pause_callback, resume_callback, add_custom_view, remove_custom_view, pause_custom_view, resume_custom_view and run_custom_view. approve_join and deny_join answer access requests from people who asked to join without an invite. A callback POSTs each new matching event to an https URL, signed with X-Tiny-Signature. A custom view POSTs the fenced code blocks of matching events to an https transform and keeps the SVG or PNG it returns as signed artifacts served at /views/<name>/<hash>.svg.",
		"- Write tools take a signed event, or return the unsigned event to sign when called with plain fields: publish_event, create_issue, create_pull_request, comment, set_status, post_message, start_thread, reply_in_thread, react, publish_wiki_page, propose_wiki_merge, publish_site, create_room, request_decision, request_job, job_feedback and job_result. A decision request is answered by a kind 7 reaction from the person asked; a job request is answered by kind 7000 feedback and a result of the request kind plus 1000. publish_site takes [path, sha256] pairs for files already in the blob store and publishes a NIP-5A manifest; an agent needs a sites grant, and a grant with a ttl requires an expiration tag.",
		"",
		"## Files",
		"",
		"- Blossom: PUT "+base+"/upload stores a file by its SHA-256 with a kind 24242 authorization; GET "+base+"/<sha256> serves it. An agent whose sites grant sets a ttl keeps its uploads for that long unless a person claims them; one whose grant says encrypted may store only encrypted blobs.",
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
		"- All guides: https://github.com/FelineStateMachine/tinyrelay/tree/main/docs",
		"",
	)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(strings.Join(lines, "\n")))
}

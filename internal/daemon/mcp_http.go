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
		Instructions: "This relay hosts Nostr events, files, Git repositories, chat rooms and a wiki, and admits agents under signed grants. Read tools return the data your key may see; management tools, including the agent controls, follow the relay's roles. Write tools accept a signed Nostr event, or build the unsigned event for you to sign when called with plain fields. request_decision asks a person for an answer that arrives as a kind 7 reaction.",
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
		"- Read tools: list_repositories, read_repository, list_issues, read_issue, list_pull_requests, read_pull_request, list_files, read_file, read_status, read_management, list_rooms, read_room, read_thread, list_wiki, read_wiki_page, read_merge_request and list_agents.",
		"- Management tools: run_job, add_job, remove_job, backup_now, dump_now, set_policy, set_connections, send_test_notification, pause_agent, resume_agent, revoke_agent, pause_all_agents and resume_all_agents.",
		"- Write tools take a signed event, or return the unsigned event to sign when called with plain fields: publish_event, create_issue, create_pull_request, comment, set_status, post_message, start_thread, reply_in_thread, react, publish_wiki_page, propose_wiki_merge, create_room and request_decision. A decision request is answered by a kind 7 reaction from the person asked.",
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

package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/mcp"
)

type mcpCall struct {
	method    string
	name      string
	arguments map[string]any
	meta      string
	version   string
	sign      bool
	secret    string
	headers   map[string]string
}

// mcpSequence keeps request bodies distinct so identical calls signed in
// the same second do not share a NIP-98 token id.
var mcpSequence int

func (c mcpCall) do(t *testing.T, app *App, path string) (*httptest.ResponseRecorder, mcp.Response) {
	t.Helper()
	mcpSequence++
	params := map[string]any{"_meta": map[string]any{mcp.MetaProtocolVersion: mcp.Version, mcp.MetaClientCapabilities: map[string]any{}}}
	if c.meta != "" {
		params["_meta"].(map[string]any)[mcp.MetaProtocolVersion] = c.meta
	}
	if c.name != "" {
		params["name"] = c.name
		params["arguments"] = c.arguments
	}
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": mcpSequence, "method": c.method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "http://relay.test"+path, strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	version := c.version
	if version == "" {
		version = mcp.Version
	}
	r.Header.Set(mcp.HeaderProtocolVersion, version)
	r.Header.Set(mcp.HeaderMethod, c.method)
	if c.name != "" {
		r.Header.Set(mcp.HeaderName, c.name)
	}
	for key, value := range c.headers {
		r.Header.Set(key, value)
	}
	if c.secret != "" {
		signRequestWithSecret(t, r, string(raw), c.secret)
	} else if c.sign {
		signRequest(t, r, string(raw))
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	var response mcp.Response
	if strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode %s: %v", w.Body.String(), err)
		}
	}
	return w, response
}

func mcpToolResult(t *testing.T, response mcp.Response) (map[string]any, bool) {
	t.Helper()
	result, _ := response.Result.(map[string]any)
	if result == nil {
		t.Fatalf("no result: %+v", response)
	}
	isError, _ := result["isError"].(bool)
	return result, isError
}

func TestMCPListsAndCallsToolsAsTheSignedKey(t *testing.T) {
	app, tenant := testTenant(t)
	policy := tenant.Policy()
	policy.Features.Grasp = true
	if err := tenant.applyPolicy(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/mcp", "/r/main/mcp"} {
		w, response := mcpCall{method: "tools/list", sign: true}.do(t, app, path)
		result, _ := response.Result.(map[string]any)
		tools, _ := result["tools"].([]any)
		if w.Code != http.StatusOK || len(tools) < 20 || result["resultType"] != "complete" || result["cacheScope"] != "private" || w.Header().Get("Mcp-Session-Id") != "" {
			t.Fatalf("%s tools/list: %d %s", path, w.Code, w.Body.String())
		}
		names := map[string]bool{}
		for _, tool := range tools {
			names[tool.(map[string]any)["name"].(string)] = true
		}
		for _, want := range []string{"list_repositories", "read_issue", "read_management", "run_job", "set_policy", "publish_event", "create_issue", "create_pull_request", "comment", "set_status"} {
			if !names[want] {
				t.Fatalf("%s missing tool %s", path, want)
			}
		}
	}
	w, response := mcpCall{method: "tools/call", name: "read_management", arguments: map[string]any{"method": "getpolicy"}, sign: true}.do(t, app, "/mcp")
	result, isError := mcpToolResult(t, response)
	structured, _ := result["structuredContent"].(map[string]any)
	policyResult, _ := structured["result"].(map[string]any)
	if w.Code != http.StatusOK || isError || policyResult["owner"] != tenant.Policy().Owner {
		t.Fatalf("read_management: %d %s", w.Code, w.Body.String())
	}
	if content, _ := result["content"].([]any); len(content) != 1 || !strings.Contains(content[0].(map[string]any)["text"].(string), tenant.Policy().Owner) {
		t.Fatalf("read_management content %v", result["content"])
	}
	w, response = mcpCall{method: "tools/call", name: "list_repositories", arguments: map[string]any{}, sign: true}.do(t, app, "/mcp")
	result, isError = mcpToolResult(t, response)
	if w.Code != http.StatusOK || isError || result["structuredContent"].(map[string]any)["next_cursor"] != "" {
		t.Fatalf("list_repositories: %d %s", w.Code, w.Body.String())
	}
	w, response = mcpCall{method: "tools/call", name: "read_file", arguments: map[string]any{"hash": "short"}, sign: true}.do(t, app, "/mcp")
	result, isError = mcpToolResult(t, response)
	if w.Code != http.StatusOK || !isError || !strings.Contains(result["content"].([]any)[0].(map[string]any)["text"].(string), "hash does not match") {
		t.Fatalf("schema validation: %d %s", w.Code, w.Body.String())
	}
}

func TestMCPWriteToolsBuildValidateAndPublish(t *testing.T) {
	app, tenant := testTenant(t)
	policy := tenant.Policy()
	policy.Features.Grasp = true
	if err := tenant.applyPolicy(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	owner := tenant.Policy().Owner
	w, response := mcpCall{method: "tools/call", name: "create_issue", arguments: map[string]any{"owner": owner, "repo": "tiny", "title": "Broken build", "content": "It fails.", "labels": []any{"bug", "bug"}}, sign: true}.do(t, app, "/mcp")
	result, isError := mcpToolResult(t, response)
	unsigned, _ := result["structuredContent"].(map[string]any)["unsigned"].(map[string]any)
	if w.Code != http.StatusOK || isError || unsigned["kind"] != float64(1621) || unsigned["content"] != "It fails." {
		t.Fatalf("create_issue template: %d %s", w.Code, w.Body.String())
	}
	tags, _ := json.Marshal(unsigned["tags"])
	if string(tags) != `[["a","30617:`+owner+`:tiny"],["p","`+owner+`"],["subject","Broken build"],["t","bug"]]` {
		t.Fatalf("create_issue tags %s", tags)
	}
	sign := func(kind int, tags [][]string, content string) map[string]any {
		e := event.Event{Kind: kind, CreatedAt: time.Now().Unix(), Tags: tags, Content: content}
		if err := event.Sign(&e, strings.Repeat("0", 63)+"1"); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(e)
		var value map[string]any
		_ = json.Unmarshal(raw, &value)
		return value
	}
	w, response = mcpCall{method: "tools/call", name: "create_issue", arguments: map[string]any{"event": sign(1, [][]string{}, "not an issue")}, sign: true}.do(t, app, "/mcp")
	result, isError = mcpToolResult(t, response)
	if w.Code != http.StatusOK || !isError || !strings.Contains(result["content"].([]any)[0].(map[string]any)["text"].(string), `Expected a signed kind 1621 event with tags ["a","30617:<owner>:<repo>"]`) {
		t.Fatalf("create_issue malformed: %d %s", w.Code, w.Body.String())
	}
	w, response = mcpCall{method: "tools/call", name: "create_issue", arguments: map[string]any{"event": sign(1621, [][]string{{"a", "30617:" + owner + ":tiny"}, {"p", owner}, {"subject", "Broken build"}}, "It fails.")}, sign: true}.do(t, app, "/mcp")
	result, isError = mcpToolResult(t, response)
	published, _ := result["structuredContent"].(map[string]any)
	if w.Code != http.StatusOK || isError || published["accepted"] != true || published["event_id"] == "" {
		t.Fatalf("create_issue publish: %d %s", w.Code, w.Body.String())
	}
	rootID, _ := published["event_id"].(string)
	w, response = mcpCall{method: "tools/call", name: "comment", arguments: map[string]any{"owner": owner, "repo": "tiny", "root": rootID, "root_kind": 1621, "root_pubkey": owner, "content": "On it."}, sign: true}.do(t, app, "/mcp")
	result, isError = mcpToolResult(t, response)
	unsigned, _ = result["structuredContent"].(map[string]any)["unsigned"].(map[string]any)
	tags, _ = json.Marshal(unsigned["tags"])
	if w.Code != http.StatusOK || isError || unsigned["kind"] != float64(1111) || string(tags) != `[["a","30617:`+owner+`:tiny"],["E","`+rootID+`","","`+owner+`"],["K","1621"],["P","`+owner+`"],["e","`+rootID+`","","`+owner+`"],["k","1621"],["p","`+owner+`"]]` {
		t.Fatalf("comment template: %d %s", w.Code, w.Body.String())
	}
	w, response = mcpCall{method: "tools/call", name: "set_status", arguments: map[string]any{"owner": owner, "repo": "tiny", "root": rootID, "root_pubkey": owner, "status": "resolved"}, sign: true}.do(t, app, "/mcp")
	result, isError = mcpToolResult(t, response)
	unsigned, _ = result["structuredContent"].(map[string]any)["unsigned"].(map[string]any)
	if w.Code != http.StatusOK || isError || unsigned["kind"] != float64(1631) {
		t.Fatalf("set_status template: %d %s", w.Code, w.Body.String())
	}
	w, response = mcpCall{method: "tools/call", name: "publish_event", arguments: map[string]any{"event": sign(1, [][]string{}, "hello agents")}, sign: true}.do(t, app, "/mcp")
	result, isError = mcpToolResult(t, response)
	if w.Code != http.StatusOK || isError || result["structuredContent"].(map[string]any)["accepted"] != true {
		t.Fatalf("publish_event: %d %s", w.Code, w.Body.String())
	}
	query := httptest.NewRequest(http.MethodPost, "http://relay.test/query", strings.NewReader(`[{"kinds":[1,1621]}]`))
	signRequest(t, query, `[{"kinds":[1,1621]}]`)
	queried := httptest.NewRecorder()
	app.ServeHTTP(queried, query)
	var rows []event.Event
	if err := json.Unmarshal(queried.Body.Bytes(), &rows); err != nil || len(rows) != 2 {
		t.Fatalf("query after publish: %s %v", queried.Body.String(), err)
	}
}

func TestMCPRejectsUnauthorizedForeignAndMismatchedRequests(t *testing.T) {
	app, _ := testTenant(t)
	w, response := mcpCall{method: "tools/list"}.do(t, app, "/mcp")
	if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != `Nostr realm="tiny"` || response.Error == nil {
		t.Fatalf("unsigned: %d %q %s", w.Code, w.Header().Get("WWW-Authenticate"), w.Body.String())
	}
	w, response = mcpCall{method: "tools/list", sign: true, headers: map[string]string{"Origin": "https://foreign.test"}}.do(t, app, "/mcp")
	if w.Code != http.StatusForbidden || response.Error == nil {
		t.Fatalf("foreign origin: %d %s", w.Code, w.Body.String())
	}
	w, _ = mcpCall{method: "tools/list", sign: true, headers: map[string]string{"Origin": "http://relay.test"}}.do(t, app, "/mcp")
	if w.Code != http.StatusOK {
		t.Fatalf("same origin: %d %s", w.Code, w.Body.String())
	}
	w, response = mcpCall{method: "tools/list", sign: true, meta: "2025-11-25"}.do(t, app, "/mcp")
	if w.Code != http.StatusBadRequest || response.Error == nil || response.Error.Code != mcp.CodeHeaderMismatch {
		t.Fatalf("version mismatch: %d %s", w.Code, w.Body.String())
	}
	w, response = mcpCall{method: "tools/list", sign: true, version: "2025-11-25", meta: "2025-11-25"}.do(t, app, "/mcp")
	if w.Code != http.StatusBadRequest || response.Error == nil || response.Error.Code != mcp.CodeUnsupportedProtocolVersion {
		t.Fatalf("unsupported version: %d %s", w.Code, w.Body.String())
	}
	w, response = mcpCall{method: "initialize", sign: true}.do(t, app, "/mcp")
	if w.Code != http.StatusNotFound || response.Error == nil || response.Error.Code != mcp.CodeMethodNotFound {
		t.Fatalf("initialize: %d %s", w.Code, w.Body.String())
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		r := httptest.NewRequest(method, "http://relay.test/mcp", nil)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "POST" {
			t.Fatalf("%s: %d", method, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "http://relay.test/r/main/llms.txt", nil)
	w = httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") || !strings.Contains(w.Body.String(), "http://relay.test/r/main/mcp") || !strings.Contains(w.Body.String(), mcp.Version) || !strings.Contains(w.Body.String(), "list_rooms") || !strings.Contains(w.Body.String(), "request_decision") {
		t.Fatalf("llms.txt: %d %s", w.Code, w.Body.String())
	}
}

// mcpSigned signs an event with the secret and returns it as the JSON object
// a client passes in the event argument.
func mcpSigned(t *testing.T, secret string, kind int, tags [][]string, content string) map[string]any {
	t.Helper()
	e := event.Event{Kind: kind, CreatedAt: time.Now().Unix(), Tags: tags, Content: content}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(e)
	var value map[string]any
	_ = json.Unmarshal(raw, &value)
	return value
}

func TestMCPRoomWikiAndAgentTools(t *testing.T) {
	app, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	member, _ := event.PublicKey(testMemberSecret)
	agent, _ := event.PublicKey(testAgentSecret)
	if _, err := tenant.Execute(ctx, owner, "setmember", []json.RawMessage{json.RawMessage(strconv.Quote(member)), json.RawMessage(`{"role":"member"}`)}); err != nil {
		t.Fatal(err)
	}
	call := func(name string, arguments map[string]any, secret string) (map[string]any, bool) {
		t.Helper()
		w, response := mcpCall{method: "tools/call", name: name, arguments: arguments, sign: true, secret: secret}.do(t, app, "/mcp")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		return mcpToolResult(t, response)
	}
	structured := func(result map[string]any) map[string]any {
		value, _ := result["structuredContent"].(map[string]any)
		return value
	}
	text := func(result map[string]any) string {
		content, _ := result["content"].([]any)
		if len(content) == 0 {
			return ""
		}
		return content[0].(map[string]any)["text"].(string)
	}
	_, response := mcpCall{method: "tools/list", sign: true}.do(t, app, "/mcp")
	names := map[string]bool{}
	for _, tool := range response.Result.(map[string]any)["tools"].([]any) {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"list_rooms", "read_room", "read_thread", "list_wiki", "read_wiki_page", "read_merge_request", "list_agents", "post_message", "start_thread", "reply_in_thread", "react", "publish_wiki_page", "propose_wiki_merge", "create_room", "pause_agent", "resume_agent", "revoke_agent", "pause_all_agents", "resume_all_agents", "request_decision"} {
		if !names[want] {
			t.Fatalf("missing tool %s", want)
		}
	}

	// Rooms: the template carries the room tags, the signed event creates
	// the room and the list shows it.
	result, isError := call("create_room", map[string]any{"room": "general", "name": "General", "about": "Talk", "visibility": "open"}, "")
	unsigned, _ := structured(result)["unsigned"].(map[string]any)
	tags, _ := json.Marshal(unsigned["tags"])
	if isError || unsigned["kind"] != float64(9007) || string(tags) != `[["h","general"],["name","General"],["about","Talk"],["visibility","open"]]` {
		t.Fatalf("create_room template: %v %s", isError, text(result))
	}
	result, isError = call("create_room", map[string]any{"event": mcpSigned(t, testOwnerSecret, 9007, [][]string{{"h", "general"}, {"name", "General"}, {"visibility", "open"}}, "")}, "")
	if isError || structured(result)["accepted"] != true {
		t.Fatalf("create_room publish: %s", text(result))
	}
	result, isError = call("list_rooms", map[string]any{}, "")
	found := false
	for _, item := range structured(result)["items"].([]any) {
		room := item.(map[string]any)
		if room["id"] == "general" && room["name"] == "General" && room["access"] == "open" {
			found = true
		}
	}
	if isError || !found {
		t.Fatalf("list_rooms: %s", text(result))
	}
	// post_message: the template, then the signed message read back.
	result, isError = call("post_message", map[string]any{"room": "general", "content": "hello room", "mentions": []any{member, member}}, testMemberSecret)
	unsigned, _ = structured(result)["unsigned"].(map[string]any)
	tags, _ = json.Marshal(unsigned["tags"])
	if isError || unsigned["kind"] != float64(9) || unsigned["content"] != "hello room" || string(tags) != `[["h","general"],["p","`+member+`"]]` {
		t.Fatalf("post_message template: %s", text(result))
	}
	result, isError = call("post_message", map[string]any{"event": mcpSigned(t, testMemberSecret, 9, [][]string{{"h", "general"}, {"p", member}}, "hello room")}, testMemberSecret)
	messageID, _ := structured(result)["event_id"].(string)
	if isError || structured(result)["accepted"] != true || messageID == "" {
		t.Fatalf("post_message publish: %s", text(result))
	}
	result, isError = call("post_message", map[string]any{"event": mcpSigned(t, testMemberSecret, 9, [][]string{}, "no room")}, testMemberSecret)
	if !isError || !strings.Contains(text(result), "the h tag must name the room") {
		t.Fatalf("post_message without room: %s", text(result))
	}
	result, isError = call("read_room", map[string]any{"id": "general"}, "")
	// The relay's own member notices (kind 44100) join the list whenever the
	// room projection has run, so count chat messages only.
	var chat []map[string]any
	items, _ := structured(result)["messages"].([]any)
	for _, item := range items {
		if m, _ := item.(map[string]any); m["kind"] == float64(9) {
			chat = append(chat, m)
		}
	}
	if isError || len(chat) != 1 || chat[0]["id"] != messageID {
		t.Fatalf("read_room: %s", text(result))
	}
	result, isError = call("reply_in_thread", map[string]any{"room": "general", "root": messageID, "root_pubkey": member, "content": "and back"}, "")
	unsigned, _ = structured(result)["unsigned"].(map[string]any)
	tags, _ = json.Marshal(unsigned["tags"])
	if isError || unsigned["kind"] != float64(12) || string(tags) != `[["h","general"],["e","`+messageID+`"],["p","`+member+`"]]` {
		t.Fatalf("reply_in_thread template: %s", text(result))
	}
	result, isError = call("reply_in_thread", map[string]any{"event": mcpSigned(t, testOwnerSecret, 12, [][]string{{"h", "general"}, {"e", messageID}, {"p", member}}, "and back")}, "")
	if isError || structured(result)["accepted"] != true {
		t.Fatalf("reply_in_thread publish: %s", text(result))
	}
	result, isError = call("read_thread", map[string]any{"id": "general", "event": messageID}, "")
	replies, _ := structured(result)["replies"].([]any)
	if isError || len(replies) != 1 || structured(result)["root"].(map[string]any)["id"] != messageID {
		t.Fatalf("read_thread: %s", text(result))
	}

	// react: content is +, - or one emoji, whether templated or signed.
	result, isError = call("react", map[string]any{"target": messageID, "target_pubkey": member, "content": "great", "room": "general"}, "")
	if !isError || !strings.Contains(text(result), "content must be +, - or one emoji") {
		t.Fatalf("react template validation: %s", text(result))
	}
	result, isError = call("react", map[string]any{"target": messageID, "target_pubkey": member, "content": "\U0001F44D", "room": "general"}, "")
	unsigned, _ = structured(result)["unsigned"].(map[string]any)
	tags, _ = json.Marshal(unsigned["tags"])
	if isError || unsigned["kind"] != float64(7) || unsigned["content"] != "\U0001F44D" || string(tags) != `[["e","`+messageID+`"],["p","`+member+`"],["h","general"]]` {
		t.Fatalf("react template: %s", text(result))
	}
	result, isError = call("react", map[string]any{"event": mcpSigned(t, testOwnerSecret, 7, [][]string{{"e", messageID}, {"p", member}, {"h", "general"}}, "great")}, "")
	if !isError || !strings.Contains(text(result), `Expected a signed kind 7 event`) {
		t.Fatalf("react signed validation: %s", text(result))
	}
	result, isError = call("react", map[string]any{"event": mcpSigned(t, testOwnerSecret, 7, [][]string{{"e", messageID}, {"p", member}, {"h", "general"}}, "+")}, "")
	if isError || structured(result)["accepted"] != true {
		t.Fatalf("react publish: %s", text(result))
	}

	// Wiki: the template normalizes the name; the signed page reads back.
	result, isError = call("publish_wiki_page", map[string]any{"d": "Bitcoin Basics", "title": "Bitcoin Basics", "summary": "A primer", "content": "Digital cash."}, "")
	unsigned, _ = structured(result)["unsigned"].(map[string]any)
	tags, _ = json.Marshal(unsigned["tags"])
	if isError || unsigned["kind"] != float64(30818) || string(tags) != `[["d","bitcoin-basics"],["title","Bitcoin Basics"],["summary","A primer"]]` {
		t.Fatalf("publish_wiki_page template: %s", text(result))
	}
	result, isError = call("publish_wiki_page", map[string]any{"event": mcpSigned(t, testOwnerSecret, 30818, [][]string{{"d", "Bitcoin Basics"}, {"title", "Bitcoin Basics"}}, "Digital cash.")}, "")
	if !isError || !strings.Contains(text(result), "normalized page name") {
		t.Fatalf("publish_wiki_page unnormalized: %s", text(result))
	}
	result, isError = call("publish_wiki_page", map[string]any{"event": mcpSigned(t, testOwnerSecret, 30818, [][]string{{"d", "bitcoin-basics"}, {"title", "Bitcoin Basics"}, {"summary", "A primer"}}, "Digital cash.")}, "")
	pageID, _ := structured(result)["event_id"].(string)
	if isError || structured(result)["accepted"] != true {
		t.Fatalf("publish_wiki_page publish: %s", text(result))
	}
	result, isError = call("read_wiki_page", map[string]any{"d": "Bitcoin Basics"}, testMemberSecret)
	page := structured(result)
	version, _ := page["version"].(map[string]any)
	if isError || page["d"] != "bitcoin-basics" || page["title"] != "Bitcoin Basics" || version["id"] != pageID || version["content"] != "Digital cash." || page["preferred_by"] != "owner" {
		t.Fatalf("read_wiki_page: %s", text(result))
	}
	result, isError = call("list_wiki", map[string]any{"q": "primer"}, "")
	if items, _ := structured(result)["items"].([]any); isError || len(items) != 1 {
		t.Fatalf("list_wiki: %s", text(result))
	}
	result, isError = call("propose_wiki_merge", map[string]any{"d": "Bitcoin Basics", "destination": owner, "source": pageID, "content": "Take my edits."}, testMemberSecret)
	unsigned, _ = structured(result)["unsigned"].(map[string]any)
	tags, _ = json.Marshal(unsigned["tags"])
	if isError || unsigned["kind"] != float64(818) || string(tags) != `[["a","30818:`+owner+`:bitcoin-basics"],["p","`+owner+`"],["e","`+pageID+`","","source"]]` {
		t.Fatalf("propose_wiki_merge template: %s", text(result))
	}
	result, isError = call("propose_wiki_merge", map[string]any{"event": mcpSigned(t, testMemberSecret, 818, [][]string{{"a", "30818:" + owner + ":bitcoin-basics"}, {"p", owner}, {"e", pageID, "", "source"}}, "Take my edits.")}, testMemberSecret)
	mergeID, _ := structured(result)["event_id"].(string)
	if isError || mergeID == "" {
		t.Fatalf("propose_wiki_merge publish: %s", text(result))
	}
	result, isError = call("read_merge_request", map[string]any{"id": mergeID}, "")
	merge, _ := structured(result)["merge"].(map[string]any)
	if isError || merge["id"] != mergeID || merge["status"] != "open" {
		t.Fatalf("read_merge_request: %s", text(result))
	}

	// Agents: owners and moderators read and control them, members do not.
	now := time.Now().Unix()
	if err := publishAs(t, tenant, agentGrantEvent(t, agent, now, now+3600, []string{"k", "1"})); err != nil {
		t.Fatal(err)
	}
	result, isError = call("list_agents", map[string]any{}, testMemberSecret)
	if !isError || !strings.Contains(text(result), "restricted") {
		t.Fatalf("list_agents as member: %v %s", isError, text(result))
	}
	result, isError = call("list_agents", map[string]any{}, "")
	agents, _ := structured(result)["result"].([]any)
	if isError || len(agents) != 1 || agents[0].(map[string]any)["pubkey"] != agent || agents[0].(map[string]any)["paused"] != false {
		t.Fatalf("list_agents as owner: %s", text(result))
	}
	result, isError = call("pause_agent", map[string]any{"agent": agent}, testMemberSecret)
	if !isError {
		t.Fatalf("pause_agent as member: %s", text(result))
	}
	result, isError = call("pause_agent", map[string]any{"agent": agent}, "")
	if isError {
		t.Fatalf("pause_agent: %s", text(result))
	}
	result, _ = call("list_agents", map[string]any{}, "")
	if agents, _ = structured(result)["result"].([]any); agents[0].(map[string]any)["paused"] != true {
		t.Fatalf("list_agents after pause: %s", text(result))
	}
	result, isError = call("resume_all_agents", map[string]any{}, "")
	if isError || structured(result)["result"].(map[string]any)["changed"] != float64(1) {
		t.Fatalf("resume_all_agents: %s", text(result))
	}
	result, isError = call("revoke_agent", map[string]any{"agent": agent}, "")
	if isError {
		t.Fatalf("revoke_agent: %s", text(result))
	}

	// request_decision: a room message or a comment carrying the request.
	result, isError = call("request_decision", map[string]any{"pubkey": member, "request": "approve", "content": "Ship 1.2?", "room": "general", "expiration": now + 3600, "subject": "Release"}, "")
	unsigned, _ = structured(result)["unsigned"].(map[string]any)
	tags, _ = json.Marshal(unsigned["tags"])
	if isError || unsigned["kind"] != float64(9) || string(tags) != `[["h","general"],["request","approve"],["p","`+member+`"],["expiration","`+strconv.FormatInt(now+3600, 10)+`"],["subject","Release"]]` {
		t.Fatalf("request_decision room template: %s", text(result))
	}
	result, isError = call("request_decision", map[string]any{"pubkey": member, "request": "question", "content": "Which name?", "root": pageID, "root_kind": 30818, "root_pubkey": owner}, "")
	unsigned, _ = structured(result)["unsigned"].(map[string]any)
	tags, _ = json.Marshal(unsigned["tags"])
	if isError || unsigned["kind"] != float64(1111) || string(tags) != `[["E","`+pageID+`","","`+owner+`"],["K","30818"],["P","`+owner+`"],["e","`+pageID+`","","`+owner+`"],["k","30818"],["p","`+owner+`"],["request","question"],["p","`+member+`"]]` {
		t.Fatalf("request_decision comment template: %s", text(result))
	}
	result, isError = call("request_decision", map[string]any{"pubkey": member, "request": "decide", "content": "Pick one."}, "")
	if !isError || !strings.Contains(text(result), "room or root is required") {
		t.Fatalf("request_decision without target: %s", text(result))
	}
	result, isError = call("request_decision", map[string]any{"event": mcpSigned(t, testOwnerSecret, 9, [][]string{{"h", "general"}, {"request", "approve"}, {"p", member}}, "Ship 1.2?")}, "")
	if isError || structured(result)["accepted"] != true {
		t.Fatalf("request_decision publish: %s", text(result))
	}
}

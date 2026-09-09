package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if c.sign {
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
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") || !strings.Contains(w.Body.String(), "http://relay.test/r/main/mcp") || !strings.Contains(w.Body.String(), mcp.Version) {
		t.Fatalf("llms.txt: %d %s", w.Code, w.Body.String())
	}
}

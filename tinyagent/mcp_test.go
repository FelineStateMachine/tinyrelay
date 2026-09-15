package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/mcp"
	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
)

// fakeRelay is a stateless MCP endpoint that checks every request the way
// the relay does and serves a small tool table.
type fakeRelay struct {
	t      *testing.T
	prefix string
	pubkey string
	mu     sync.Mutex
	calls  []string
	events []nostr.Event
	server *httptest.Server
}

var fakeTools = []map[string]any{
	{"name": "list_rooms", "description": "List rooms.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}, "annotations": map[string]any{"readOnlyHint": true}},
	{"name": "read_room", "description": "Read a room.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []any{"id"}}},
	{"name": "post_message", "description": "Post a message.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"event": map[string]any{"type": "object"}, "room": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}}},
	{"name": "request_job", "description": "Ask for a long task.", "inputSchema": map[string]any{"type": "object"}},
	{"name": "publish_event", "description": "Publish any event.", "inputSchema": map[string]any{"type": "object"}},
	{"name": "set_policy", "description": "Change policy.", "inputSchema": map[string]any{"type": "object"}},
}

func newFakeRelay(t *testing.T, prefix, pubkey string) *fakeRelay {
	f := &fakeRelay{t: t, prefix: prefix, pubkey: pubkey}
	f.server = httptest.NewServer(f)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeRelay) base() string { return f.server.URL + f.prefix }

func (f *fakeRelay) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeRelay) eventList() []nostr.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]nostr.Event(nil), f.events...)
}

func (f *fakeRelay) fail(w http.ResponseWriter, status, code int, message string) {
	f.t.Errorf("fake relay refused a request: %d %s", status, message)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(mcp.Response{JSONRPC: "2.0", ID: json.RawMessage("1"), Error: &mcp.Error{Code: code, Message: message}})
}

func (f *fakeRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != f.prefix+"/mcp" {
		f.fail(w, http.StatusNotFound, mcp.CodeInvalidRequest, "wrong method or path: "+r.Method+" "+r.URL.Path)
		return
	}
	body, _ := io.ReadAll(r.Body)
	if err := f.checkProof(r, body); err != nil {
		f.fail(w, http.StatusUnauthorized, mcp.CodeInvalidRequest, err.Error())
		return
	}
	if r.Header.Get("Content-Type") != "application/json" || !strings.Contains(r.Header.Get("Accept"), "application/json") {
		f.fail(w, http.StatusBadRequest, mcp.CodeInvalidRequest, "content negotiation headers")
		return
	}
	var request mcp.Request
	if err := json.Unmarshal(body, &request); err != nil || request.JSONRPC != "2.0" {
		f.fail(w, http.StatusBadRequest, mcp.CodeParse, "body")
		return
	}
	var params struct {
		Meta      map[string]json.RawMessage `json:"_meta"`
		Name      string                     `json:"name"`
		Arguments map[string]any             `json:"arguments"`
	}
	_ = json.Unmarshal(request.Params, &params)
	if r.Header.Get(mcp.HeaderProtocolVersion) != mcp.Version || string(params.Meta[mcp.MetaProtocolVersion]) != `"`+mcp.Version+`"` {
		f.fail(w, http.StatusBadRequest, mcp.CodeHeaderMismatch, "protocol version")
		return
	}
	if string(params.Meta[mcp.MetaClientCapabilities]) != "{}" || !bytes.Contains(params.Meta[mcp.MetaClientInfo], []byte(`"tinyagent"`)) {
		f.fail(w, http.StatusBadRequest, mcp.CodeInvalidParams, "_meta")
		return
	}
	if r.Header.Get(mcp.HeaderMethod) != request.Method {
		f.fail(w, http.StatusBadRequest, mcp.CodeHeaderMismatch, "Mcp-Method")
		return
	}
	if request.Method == "tools/call" {
		if name, err := mcp.DecodeHeaderValue(r.Header.Get(mcp.HeaderName)); err != nil || name != params.Name {
			f.fail(w, http.StatusBadRequest, mcp.CodeHeaderMismatch, "Mcp-Name")
			return
		}
	}
	answer := func(status int, result any, rpcErr *mcp.Error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(mcp.Response{JSONRPC: "2.0", ID: request.ID, Result: result, Error: rpcErr})
	}
	switch request.Method {
	case "tools/list":
		answer(http.StatusOK, map[string]any{"tools": fakeTools, "ttlMs": 300000, "cacheScope": "private"}, nil)
	case "tools/call":
		f.mu.Lock()
		f.calls = append(f.calls, params.Name)
		f.mu.Unlock()
		f.callTool(answer, params.Name, params.Arguments)
	default:
		answer(http.StatusNotFound, nil, &mcp.Error{Code: mcp.CodeMethodNotFound, Message: "method " + request.Method + " is not supported"})
	}
}

func (f *fakeRelay) checkProof(r *http.Request, body []byte) error {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Nostr ") {
		return fmt.Errorf("missing Nostr authorization")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Nostr "))
	if err != nil {
		return err
	}
	e, err := nostr.Parse(raw)
	if err != nil {
		return err
	}
	if err := nostr.Validate(e); err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	switch {
	case e.Kind != 27235:
		return fmt.Errorf("kind %d", e.Kind)
	case e.PubKey != f.pubkey:
		return fmt.Errorf("signer %s", e.PubKey)
	case nostr.Tag(e, "u") != f.server.URL+r.URL.RequestURI():
		return fmt.Errorf("u tag %s", nostr.Tag(e, "u"))
	case nostr.Tag(e, "method") != http.MethodPost:
		return fmt.Errorf("method tag %s", nostr.Tag(e, "method"))
	case nostr.Tag(e, "payload") != hex.EncodeToString(sum[:]):
		return fmt.Errorf("payload tag")
	case time.Now().Unix()-e.CreatedAt > 60:
		return fmt.Errorf("stale token")
	}
	return nil
}

func (f *fakeRelay) callTool(answer func(int, any, *mcp.Error), name string, arguments map[string]any) {
	ok := func(structured any) {
		text, _ := json.Marshal(structured)
		answer(http.StatusOK, map[string]any{"resultType": "complete", "content": []any{map[string]any{"type": "text", "text": string(text)}}, "structuredContent": structured, "isError": false}, nil)
	}
	failure := func(message string, structured any) {
		answer(http.StatusOK, map[string]any{"resultType": "complete", "content": []any{map[string]any{"type": "text", "text": message}}, "structuredContent": structured, "isError": true}, nil)
	}
	switch name {
	case "list_rooms":
		if arguments["q"] == "secret" {
			failure("restricted: members only", nil)
			return
		}
		ok(map[string]any{"rooms": []any{map[string]any{"id": "lobby", "url": f.base() + "/chat/lobby", "address": "/chat/lobby"}}})
	case "read_room":
		switch arguments["id"] {
		case "forbidden":
			answer(http.StatusForbidden, nil, &mcp.Error{Code: mcp.CodeInvalidRequest, Message: "origin is not allowed"})
		case "unauthorized":
			answer(http.StatusUnauthorized, nil, &mcp.Error{Code: mcp.CodeInvalidRequest, Message: "auth-required: sign this request"})
		case "boom":
			answer(http.StatusInternalServerError, nil, &mcp.Error{Code: mcp.CodeInternal, Message: "store unavailable"})
		case "huge":
			ok(map[string]any{"text": strings.Repeat("x", 100<<10)})
		case "rejected":
			failure("The relay rejected the event: restricted: room is read-only", map[string]any{"accepted": false, "message": "restricted: room is read-only"})
		default:
			ok(map[string]any{"id": arguments["id"], "messages": []any{}})
		}
	case "post_message":
		if raw, present := arguments["event"]; present {
			encoded, _ := json.Marshal(raw)
			e, err := nostr.Parse(encoded)
			if err == nil {
				err = nostr.Validate(e)
			}
			if err != nil {
				failure("The event is not acceptable: "+err.Error(), nil)
				return
			}
			f.mu.Lock()
			f.events = append(f.events, e)
			f.mu.Unlock()
			ok(map[string]any{"event_id": e.ID, "accepted": true, "message": ""})
			return
		}
		room, _ := arguments["room"].(string)
		content, _ := arguments["content"].(string)
		if room == "" || content == "" {
			failure("room and content are required", nil)
			return
		}
		if room == "auth" {
			ok(map[string]any{"unsigned": map[string]any{"kind": 27235, "created_at": time.Now().Unix(), "tags": []any{[]any{"u", "https://elsewhere.test/"}}, "content": ""}, "next": "sign"})
			return
		}
		ok(map[string]any{"unsigned": map[string]any{"kind": 9, "created_at": time.Now().Unix(), "tags": []any{[]any{"h", room}, []any{"p", f.pubkey}}, "content": content}, "next": "Sign this event and call again with event."})
	default:
		answer(http.StatusBadRequest, nil, &mcp.Error{Code: mcp.CodeInvalidParams, Message: "Unknown tool: " + name})
	}
}

// facadeConn drives mcpCommand over in-memory pipes as a scripted client.
type facadeConn struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *bufio.Reader
	stderr *bytes.Buffer
	done   chan error
	next   int
}

func startFacade(t *testing.T, args ...string) *facadeConn {
	t.Helper()
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	c := &facadeConn{t: t, in: inWriter, out: bufio.NewReaderSize(outReader, 1<<20), stderr: &bytes.Buffer{}, done: make(chan error, 1)}
	go func() {
		err := mcpCommand(args, inReader, outWriter, c.stderr)
		outWriter.Close()
		c.done <- err
	}()
	t.Cleanup(func() {
		inWriter.Close()
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
		}
	})
	return c
}

func (c *facadeConn) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

func (c *facadeConn) read() mcp.Response {
	c.t.Helper()
	line, err := c.out.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("read response: %v (stderr: %s)", err, c.stderr.String())
	}
	var response mcp.Response
	if err := json.Unmarshal(line, &response); err != nil {
		c.t.Fatalf("invalid response %s: %v", line, err)
	}
	return response
}

func (c *facadeConn) call(method string, params any) mcp.Response {
	c.t.Helper()
	c.next++
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.next, "method": method, "params": params})
	c.send(string(raw))
	response := c.read()
	if string(response.ID) != fmt.Sprint(c.next) {
		c.t.Fatalf("response id %s for request %d", response.ID, c.next)
	}
	return response
}

func (c *facadeConn) initialize(version string) map[string]any {
	c.t.Helper()
	response := c.call("initialize", map[string]any{"protocolVersion": version, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "mcp", "version": "1.26.0"}})
	if response.Error != nil {
		c.t.Fatalf("initialize: %v", response.Error)
	}
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	result, _ := response.Result.(map[string]any)
	return result
}

func (c *facadeConn) listNames() []string {
	c.t.Helper()
	response := c.call("tools/list", map[string]any{})
	if response.Error != nil {
		c.t.Fatalf("tools/list: %v", response.Error)
	}
	tools, _ := response.Result.(map[string]any)["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		name, _ := tool.(map[string]any)["name"].(string)
		names = append(names, name)
	}
	return names
}

func (c *facadeConn) tool(name string, arguments map[string]any) (mcpToolResult, mcp.Response) {
	c.t.Helper()
	response := c.call("tools/call", map[string]any{"name": name, "arguments": arguments})
	var result mcpToolResult
	if response.Error == nil {
		raw, _ := json.Marshal(response.Result)
		if err := json.Unmarshal(raw, &result); err != nil {
			c.t.Fatalf("tool result %s: %v", raw, err)
		}
	}
	return result, response
}

func firstText(result mcpToolResult) string {
	if len(result.Content) == 0 {
		return ""
	}
	text, _ := result.Content[0]["text"].(string)
	return text
}

func testKey(t *testing.T) (string, string) {
	t.Helper()
	secret, err := nostr.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := nostr.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TINY_MCP_TEST_KEY", secret)
	return secret, pub
}

func TestMCPInitializeAndFilteredList(t *testing.T) {
	_, pub := testKey(t)
	relay := newFakeRelay(t, "", pub)
	c := startFacade(t, "--relay", relay.base(), "--key-env", "TINY_MCP_TEST_KEY")
	result := c.initialize("2025-11-25")
	if result["protocolVersion"] != "2025-11-25" {
		t.Fatalf("protocol version %v", result["protocolVersion"])
	}
	if capabilities, _ := result["capabilities"].(map[string]any); capabilities["tools"] == nil {
		t.Fatalf("capabilities %v", result["capabilities"])
	}
	if info, _ := result["serverInfo"].(map[string]any); info["name"] != "tinyagent" {
		t.Fatalf("serverInfo %v", result["serverInfo"])
	}
	if ping := c.call("ping", nil); ping.Error != nil || ping.Result == nil {
		t.Fatalf("ping: %+v", ping)
	}
	names := c.listNames()
	if strings.Join(names, ",") != "list_rooms,read_room,tiny_diagnose" {
		t.Fatalf("read-only table: %v", names)
	}
	response := c.call("tools/list", map[string]any{})
	tool := response.Result.(map[string]any)["tools"].([]any)[0].(map[string]any)
	if tool["description"] != "List rooms." || tool["inputSchema"] == nil || tool["annotations"] == nil {
		t.Fatalf("relay schema not passed through: %v", tool)
	}
	if unknown := c.call("resources/list", nil); unknown.Error == nil || unknown.Error.Code != mcp.CodeMethodNotFound {
		t.Fatalf("unknown method: %+v", unknown)
	}
	c.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	c.send(`not json`)
	if parse := c.read(); parse.Error == nil || parse.Error.Code != mcp.CodeParse {
		t.Fatalf("parse error: %+v", parse)
	}
	if ping := c.call("ping", nil); ping.Error != nil {
		t.Fatalf("ping after notifications: %+v", ping)
	}
	if strings.Contains(c.stderr.String(), "\"jsonrpc\"") {
		t.Fatalf("protocol output on stderr: %s", c.stderr.String())
	}
}

func TestMCPInitializeFallsBackToKnownVersion(t *testing.T) {
	_, pub := testKey(t)
	relay := newFakeRelay(t, "", pub)
	c := startFacade(t, "--relay", relay.base(), "--key-env", "TINY_MCP_TEST_KEY")
	if result := c.initialize("2030-01-01"); result["protocolVersion"] != mcpFallbackVersion {
		t.Fatalf("fallback version %v", result["protocolVersion"])
	}
}

func TestMCPAllowWritesAndTenantPrefix(t *testing.T) {
	_, pub := testKey(t)
	relay := newFakeRelay(t, "/r/acme", pub)
	c := startFacade(t, "--relay", relay.base(), "--key-env", "TINY_MCP_TEST_KEY", "--allow-writes")
	c.initialize("2025-06-18")
	names := c.listNames()
	if strings.Join(names, ",") != "list_rooms,read_room,post_message,request_job,tiny_diagnose" {
		t.Fatalf("write table: %v", names)
	}
	for _, name := range []string{"publish_event", "set_policy"} {
		if _, response := c.tool(name, nil); response.Error == nil || response.Error.Code != mcp.CodeInvalidParams {
			t.Fatalf("%s must stay hidden: %+v", name, response)
		}
	}
	if len(relay.callList()) != 0 {
		t.Fatalf("hidden tools reached the relay: %v", relay.callList())
	}
}

func TestMCPCallSignsAndResubmits(t *testing.T) {
	_, pub := testKey(t)
	relay := newFakeRelay(t, "", pub)
	c := startFacade(t, "--relay", relay.base(), "--key-env", "TINY_MCP_TEST_KEY", "--allow-writes")
	c.initialize("2025-06-18")
	result, response := c.tool("post_message", map[string]any{"room": "lobby", "content": "hello"})
	if response.Error != nil || result.IsError {
		t.Fatalf("post_message: %+v %+v", response.Error, result)
	}
	structured, _ := result.StructuredContent.(map[string]any)
	if structured["accepted"] != true || structured["event_id"] == "" {
		t.Fatalf("publish receipt: %v", structured)
	}
	if strings.Join(relay.callList(), ",") != "post_message,post_message" {
		t.Fatalf("relay calls: %v", relay.callList())
	}
	if len(relay.eventList()) != 1 {
		t.Fatalf("published events: %d", len(relay.eventList()))
	}
	e := relay.eventList()[0]
	tags, _ := json.Marshal(e.Tags)
	if e.Kind != 9 || e.PubKey != pub || e.Content != "hello" || string(tags) != `[["h","lobby"],["p","`+pub+`"]]` || e.ID != structured["event_id"] {
		t.Fatalf("signed event differs from the template: %+v", e)
	}
	// A read result passes through with its URLs and path addresses intact.
	result, _ = c.tool("list_rooms", map[string]any{})
	rooms, _ := result.StructuredContent.(map[string]any)["rooms"].([]any)
	room, _ := rooms[0].(map[string]any)
	if result.IsError || room["url"] != relay.base()+"/chat/lobby" || room["address"] != "/chat/lobby" {
		t.Fatalf("list_rooms: %+v", result)
	}
	// A template of an authorization kind is never signed.
	result, _ = c.tool("post_message", map[string]any{"room": "auth", "content": "x"})
	if !result.IsError || !strings.Contains(firstText(result), "refused to sign a kind 27235") || len(relay.eventList()) != 1 {
		t.Fatalf("auth template: %+v", result)
	}
}

func TestMCPPermissionAndTransportErrors(t *testing.T) {
	_, pub := testKey(t)
	relay := newFakeRelay(t, "", pub)
	c := startFacade(t, "--relay", relay.base(), "--key-env", "TINY_MCP_TEST_KEY")
	c.initialize("2025-06-18")
	cases := map[string]struct {
		tool   string
		args   map[string]any
		prefix string
	}{
		"restricted result":   {"list_rooms", map[string]any{"q": "secret"}, "permission: restricted: members only"},
		"structured message":  {"read_room", map[string]any{"id": "rejected"}, "permission: restricted: room is read-only"},
		"http 401":            {"read_room", map[string]any{"id": "unauthorized"}, "permission: auth-required: sign this request"},
		"http 403":            {"read_room", map[string]any{"id": "forbidden"}, "permission: origin is not allowed"},
		"http 500":            {"read_room", map[string]any{"id": "boom"}, "transport: relay HTTP 500"},
		"plain relay failure": {"read_room", map[string]any{}, ""},
	}
	for name, tc := range cases {
		result, response := c.tool(tc.tool, tc.args)
		if response.Error != nil {
			t.Fatalf("%s: JSON-RPC error %+v", name, response.Error)
		}
		if tc.prefix == "" {
			if result.IsError {
				t.Fatalf("%s: unexpected error %+v", name, result)
			}
			continue
		}
		if !result.IsError || !strings.HasPrefix(firstText(result), tc.prefix) {
			t.Fatalf("%s: %+v", name, result)
		}
	}
	result, _ := c.tool("read_room", map[string]any{"id": "huge"})
	text := firstText(result)
	if result.IsError || len(text) > mcpTextLimit+100 || !strings.Contains(text, "[truncated: ") {
		t.Fatalf("huge text not bounded: %d bytes, error %v", len(text), result.IsError)
	}
	relay.server.Close()
	result, response := c.tool("read_room", map[string]any{"id": "lobby"})
	if response.Error != nil || !result.IsError || !strings.HasPrefix(firstText(result), "transport: ") {
		t.Fatalf("connection refused: %+v %+v", response.Error, result)
	}
}

func TestMCPListFailsWhenRelayIsDown(t *testing.T) {
	_, pub := testKey(t)
	relay := newFakeRelay(t, "", pub)
	c := startFacade(t, "--relay", relay.base(), "--key-env", "TINY_MCP_TEST_KEY")
	c.initialize("2025-06-18")
	relay.server.Close()
	response := c.call("tools/list", map[string]any{})
	if response.Error == nil || response.Error.Code != mcp.CodeInternal || !strings.HasPrefix(response.Error.Message, "transport: ") {
		t.Fatalf("tools/list while down: %+v", response)
	}
}

func TestMCPToolsFlag(t *testing.T) {
	_, pub := testKey(t)
	relay := newFakeRelay(t, "", pub)
	c := startFacade(t, "--relay", relay.base(), "--key-env", "TINY_MCP_TEST_KEY", "--tools", "read_room, list_rooms,list_wiki")
	c.initialize("2025-06-18")
	if names := c.listNames(); strings.Join(names, ",") != "read_room,list_rooms,tiny_diagnose" {
		t.Fatalf("--tools table: %v", names)
	}
	for _, tc := range []struct{ tools, want string }{
		{"list_rooms,publish_event", `unknown tool "publish_event"`},
		{"set_policy", `unknown tool "set_policy"`},
		{"post_message", "add --allow-writes"},
		{" , ", "names no tool"},
	} {
		err := mcpCommand([]string{"--relay", relay.base(), "--key-env", "TINY_MCP_TEST_KEY", "--tools", tc.tools}, strings.NewReader(""), io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("--tools %q: %v", tc.tools, err)
		}
	}
	order, err := mcpAllowlist("post_message,list_rooms", true)
	if err != nil || strings.Join(order, ",") != "post_message,list_rooms" {
		t.Fatalf("write tool with --allow-writes: %v %v", order, err)
	}
	if err := mcpCommand([]string{"--relay", relay.base(), "--key-env", "TINY_MCP_MISSING"}, strings.NewReader(""), io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "TINY_MCP_MISSING is not set") {
		t.Fatalf("missing key: %v", err)
	}
	if err := run([]string{"mcp"}, strings.NewReader(""), io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "--relay") {
		t.Fatalf("missing relay: %v", err)
	}
}

func TestMCPCatalogNeverNamesManagementTools(t *testing.T) {
	for _, name := range append(append([]string(nil), mcpReadTools...), mcpWriteTools...) {
		if name == "publish_event" || name == "set_policy" || name == "read_management" || strings.HasSuffix(name, "_agent") || strings.HasSuffix(name, "_callback") {
			t.Fatalf("catalog exposes %s", name)
		}
	}
}

// TestMCPAgainstRelayTransport runs the facade against the relay's own MCP
// transport so the headers, _meta and NIP-98 proof it sends are checked by
// the same code the daemon uses.
func TestMCPAgainstRelayTransport(t *testing.T) {
	_, pub := testKey(t)
	registry := &mcp.Registry{}
	var published []nostr.Event
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(registry.Add(mcp.Tool{Name: "list_rooms", Description: "List rooms.", InputSchema: mcp.Object(map[string]any{"q": map[string]any{"type": "string"}}), Handler: func(_ context.Context, call mcp.Call) (mcp.Result, error) {
		if call.String("q") == "secret" {
			return mcp.Result{}, errors.New("restricted: members only")
		}
		return mcp.Value(map[string]any{"rooms": []any{"lobby"}, "actor": call.Actor}), nil
	}}))
	must(registry.Add(mcp.Tool{Name: "post_message", Description: "Post.", InputSchema: mcp.Object(map[string]any{"event": map[string]any{"type": "object"}, "room": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}), Handler: func(_ context.Context, call mcp.Call) (mcp.Result, error) {
		if raw, present := call.Arguments["event"]; present {
			encoded, _ := json.Marshal(raw)
			e, err := nostr.Parse(encoded)
			if err == nil {
				err = nostr.Validate(e)
			}
			if err != nil || e.Kind != 9 || e.PubKey != call.Actor {
				return mcp.Failure("The event is not acceptable.", nil), nil
			}
			published = append(published, e)
			return mcp.Value(map[string]any{"event_id": e.ID, "accepted": true, "message": ""}), nil
		}
		return mcp.Value(map[string]any{"unsigned": map[string]any{"kind": 9, "created_at": time.Now().Unix(), "tags": [][]string{{"h", call.String("room")}}, "content": call.String("content")}, "next": "sign"}), nil
	}}))
	must(registry.Add(mcp.Tool{Name: "publish_event", Description: "Any event.", InputSchema: mcp.Object(nil), Handler: func(context.Context, mcp.Call) (mcp.Result, error) {
		t.Fatal("publish_event must never be reached")
		return mcp.Result{}, nil
	}}))
	var server *httptest.Server
	relay := &mcp.Server{Tools: registry, Info: mcp.Implementation{Name: "tinyrelay", Version: "test"}, Challenge: `Nostr realm="tiny"`, Authenticate: func(r *http.Request, body []byte) (string, error) {
		fake := &fakeRelay{t: t, pubkey: pub, server: server}
		if err := fake.checkProof(r, body); err != nil {
			return "", errors.New("auth-required: " + err.Error())
		}
		return pub, nil
	}}
	server = httptest.NewServer(relay)
	t.Cleanup(server.Close)
	c := startFacade(t, "--relay", server.URL, "--key-env", "TINY_MCP_TEST_KEY", "--allow-writes")
	c.initialize("2025-03-26")
	if names := c.listNames(); strings.Join(names, ",") != "list_rooms,post_message,tiny_diagnose" {
		t.Fatalf("table: %v", names)
	}
	result, response := c.tool("list_rooms", map[string]any{})
	if response.Error != nil || result.IsError || result.StructuredContent.(map[string]any)["actor"] != pub {
		t.Fatalf("list_rooms: %+v %+v", response.Error, result)
	}
	result, _ = c.tool("list_rooms", map[string]any{"q": "secret"})
	if !result.IsError || firstText(result) != "permission: restricted: members only" {
		t.Fatalf("restricted: %+v", result)
	}
	result, _ = c.tool("list_rooms", map[string]any{"q": 7})
	if !result.IsError || strings.HasPrefix(firstText(result), "permission:") || strings.HasPrefix(firstText(result), "transport:") {
		t.Fatalf("schema failure should pass through: %+v", result)
	}
	result, _ = c.tool("post_message", map[string]any{"room": "lobby", "content": "hi"})
	if result.IsError || len(published) != 1 || published[0].Content != "hi" || result.StructuredContent.(map[string]any)["accepted"] != true {
		t.Fatalf("sign and resubmit: %+v", result)
	}
	// The wrong key is a 401 from the transport, reported as a permission error.
	other, err := nostr.GenerateKey()
	must(err)
	t.Setenv("TINY_MCP_OTHER_KEY", other)
	stranger := startFacade(t, "--relay", server.URL, "--key-env", "TINY_MCP_OTHER_KEY")
	stranger.initialize("2025-06-18")
	if response := stranger.call("tools/list", nil); response.Error == nil || !strings.HasPrefix(response.Error.Message, "permission: auth-required:") {
		t.Fatalf("unauthorized list: %+v", response)
	}
}

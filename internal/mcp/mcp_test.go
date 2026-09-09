package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	registry := &Registry{}
	if err := registry.Add(Tool{Name: "echo", Description: "Echo the message.", InputSchema: Object(map[string]any{"message": map[string]any{"type": "string", "minLength": 1}, "count": map[string]any{"type": "integer", "minimum": 1, "maximum": 3}}, "message"), Annotations: map[string]any{"readOnlyHint": true}, Handler: func(ctx context.Context, call Call) (Result, error) {
		if call.String("message") == "fail" {
			return Result{}, errors.New("restricted: the message may not fail")
		}
		return Value(map[string]any{"actor": call.Actor, "message": call.String("message")}), nil
	}}); err != nil {
		t.Fatal(err)
	}
	return &Server{Tools: registry, Info: Implementation{Name: "test", Version: "1"}, Challenge: `Nostr realm="tiny"`,
		Authenticate: func(r *http.Request, body []byte) (string, error) {
			if r.Header.Get("Authorization") != "Nostr ok" {
				return "", errors.New("auth-required: sign this request")
			}
			return "actor", nil
		},
		Origin: func(*http.Request) string { return "http://relay.test" }}
}

type call struct {
	method, name, version, metaVersion, body string
	headers                                  map[string]string
	skipHeaders                              bool
}

func (c call) request() *http.Request {
	body := c.body
	if body == "" {
		params := map[string]any{"_meta": map[string]any{MetaClientCapabilities: map[string]any{}}}
		if c.metaVersion != "-" {
			version := c.metaVersion
			if version == "" {
				version = Version
			}
			params["_meta"].(map[string]any)[MetaProtocolVersion] = version
		}
		if c.name != "" {
			params["name"] = c.name
			params["arguments"] = map[string]any{"message": "hi"}
		}
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": c.method, "params": params})
		body = string(raw)
	}
	r := httptest.NewRequest(http.MethodPost, "http://relay.test/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("Authorization", "Nostr ok")
	if !c.skipHeaders {
		version := c.version
		if version == "" {
			version = Version
		}
		r.Header.Set(HeaderProtocolVersion, version)
		r.Header.Set(HeaderMethod, c.method)
		if c.name != "" {
			r.Header.Set(HeaderName, EncodeHeaderValue(c.name))
		}
	}
	for key, value := range c.headers {
		r.Header.Set(key, value)
	}
	return r
}

func serve(t *testing.T, s *Server, r *http.Request) (*httptest.ResponseRecorder, Response, Observation) {
	t.Helper()
	w := httptest.NewRecorder()
	obs := s.Handle(w, r)
	var response Response
	if w.Body.Len() > 0 && strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode %s: %v", w.Body.String(), err)
		}
	}
	return w, response, obs
}

func TestOnlyPostIsServed(t *testing.T) {
	s := testServer(t)
	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		w, _, obs := serve(t, s, httptest.NewRequest(method, "http://relay.test/mcp", nil))
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "POST" || obs.Outcome != OutcomeInvalid {
			t.Fatalf("%s: %d %q %+v", method, w.Code, w.Header().Get("Allow"), obs)
		}
	}
}

func TestOriginAndAuthorization(t *testing.T) {
	s := testServer(t)
	w, response, obs := serve(t, s, call{method: "tools/list", headers: map[string]string{"Origin": "https://evil.test"}}.request())
	if w.Code != http.StatusForbidden || response.Error == nil || len(response.ID) != 0 || obs.Outcome != OutcomeInvalid {
		t.Fatalf("foreign origin: %d %s", w.Code, w.Body.String())
	}
	w, _, _ = serve(t, s, call{method: "tools/list", headers: map[string]string{"Origin": "HTTP://relay.test"}}.request())
	if w.Code != http.StatusOK {
		t.Fatalf("same origin: %d %s", w.Code, w.Body.String())
	}
	w, response, obs = serve(t, s, call{method: "tools/list", headers: map[string]string{"Authorization": "Nostr bad"}}.request())
	if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != `Nostr realm="tiny"` || response.Error == nil || obs.Outcome != OutcomeUnauthorized {
		t.Fatalf("unauthorized: %d %s %q", w.Code, w.Body.String(), w.Header().Get("WWW-Authenticate"))
	}
}

func TestHeaderValidation(t *testing.T) {
	s := testServer(t)
	for _, tc := range []struct {
		name string
		call call
		code int
	}{
		{"missing version header", call{method: "tools/list", headers: map[string]string{HeaderProtocolVersion: ""}}, CodeHeaderMismatch},
		{"unsupported version", call{method: "tools/list", version: "2025-11-25", metaVersion: "2025-11-25"}, CodeUnsupportedProtocolVersion},
		{"meta version mismatch", call{method: "tools/list", metaVersion: "2025-11-25"}, CodeHeaderMismatch},
		{"meta version missing", call{method: "tools/list", metaVersion: "-"}, CodeInvalidParams},
		{"method header mismatch", call{method: "tools/list", headers: map[string]string{HeaderMethod: "tools/call"}}, CodeHeaderMismatch},
		{"method header missing", call{method: "tools/list", headers: map[string]string{HeaderMethod: ""}}, CodeHeaderMismatch},
		{"name header missing", call{method: "tools/call", name: "echo", headers: map[string]string{HeaderName: ""}}, CodeHeaderMismatch},
		{"name header mismatch", call{method: "tools/call", name: "echo", headers: map[string]string{HeaderName: "other"}}, CodeHeaderMismatch},
		{"name header bad base64", call{method: "tools/call", name: "echo", headers: map[string]string{HeaderName: "=?base64?***?="}}, CodeHeaderMismatch},
		{"no headers at all", call{method: "tools/list", skipHeaders: true}, CodeHeaderMismatch},
		{"missing capabilities", call{method: "tools/list", body: `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"` + MetaProtocolVersion + `":"` + Version + `"}}}`}, CodeInvalidParams},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, response, obs := serve(t, s, tc.call.request())
			if w.Code != http.StatusBadRequest || response.Error == nil || response.Error.Code != tc.code || obs.Outcome != OutcomeInvalid {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if tc.code == CodeUnsupportedProtocolVersion {
				data, _ := response.Error.Data.(map[string]any)
				if data["requested"] != "2025-11-25" || len(data["supported"].([]any)) != 1 {
					t.Fatalf("unsupported version data %v", response.Error.Data)
				}
			}
		})
	}
	// Encoded names decode before comparison, and a stale session header is ignored.
	w, _, _ := serve(t, s, call{method: "tools/call", name: "echo", headers: map[string]string{HeaderName: "=?base64?ZWNobw==?=", "Mcp-Session-Id": "stale", "Last-Event-ID": "3"}}.request())
	if w.Code != http.StatusOK || w.Header().Get("Mcp-Session-Id") != "" {
		t.Fatalf("encoded name: %d %s", w.Code, w.Body.String())
	}
}

func TestBodyValidation(t *testing.T) {
	s := testServer(t)
	for _, tc := range []struct {
		name   string
		body   string
		status int
		code   int
	}{
		{"parse error", `{"jsonrpc":`, http.StatusBadRequest, CodeParse},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, http.StatusBadRequest, CodeParse},
		{"wrong version", `{"jsonrpc":"1.0","id":1,"method":"tools/list"}`, http.StatusBadRequest, CodeInvalidRequest},
		{"null id", `{"jsonrpc":"2.0","id":null,"method":"tools/list"}`, http.StatusBadRequest, CodeInvalidRequest},
		{"params not object", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":[]}`, http.StatusBadRequest, CodeInvalidParams},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, response, _ := serve(t, s, call{method: "tools/list", body: tc.body}.request())
			if w.Code != tc.status || response.Error == nil || response.Error.Code != tc.code {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
	w, _, obs := serve(t, s, call{method: "notifications/cancelled", body: `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`}.request())
	if w.Code != http.StatusAccepted || w.Body.Len() != 0 || obs.Outcome != OutcomeOK {
		t.Fatalf("notification: %d %q", w.Code, w.Body.String())
	}
	w, _, _ = serve(t, s, call{method: "tools/list", headers: map[string]string{"Accept": "text/html"}}.request())
	if w.Code != http.StatusNotAcceptable {
		t.Fatalf("accept: %d", w.Code)
	}
	w, _, _ = serve(t, s, call{method: "tools/list", headers: map[string]string{"Content-Type": "text/plain"}}.request())
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content type: %d", w.Code)
	}
}

func TestRoutingAndResults(t *testing.T) {
	s := testServer(t)
	w, response, obs := serve(t, s, call{method: "server/discover"}.request())
	result, _ := response.Result.(map[string]any)
	if w.Code != http.StatusOK || result["resultType"] != "complete" || obs.Outcome != OutcomeOK {
		t.Fatalf("discover: %d %s", w.Code, w.Body.String())
	}
	if versions, _ := result["supportedVersions"].([]any); len(versions) != 1 || versions[0] != Version {
		t.Fatalf("discover versions %v", result["supportedVersions"])
	}
	if meta, _ := result["_meta"].(map[string]any); meta[MetaServerInfo].(map[string]any)["name"] != "test" {
		t.Fatalf("discover meta %v", result["_meta"])
	}
	w, response, _ = serve(t, s, call{method: "tools/list"}.request())
	result, _ = response.Result.(map[string]any)
	tools, _ := result["tools"].([]any)
	if w.Code != http.StatusOK || len(tools) != 1 || result["cacheScope"] != "private" || result["ttlMs"] == nil || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if tool := tools[0].(map[string]any); tool["name"] != "echo" || tool["inputSchema"].(map[string]any)["type"] != "object" || tool["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Fatalf("tool %v", tool)
	}
	w, response, obs = serve(t, s, call{method: "tools/call", name: "echo"}.request())
	result, _ = response.Result.(map[string]any)
	if w.Code != http.StatusOK || result["isError"] != false || obs.Tool != "echo" || obs.Outcome != OutcomeOK {
		t.Fatalf("call: %d %s", w.Code, w.Body.String())
	}
	structured, _ := result["structuredContent"].(map[string]any)
	content, _ := result["content"].([]any)
	if structured["actor"] != "actor" || structured["message"] != "hi" || len(content) != 1 || !strings.Contains(content[0].(map[string]any)["text"].(string), `"message":"hi"`) {
		t.Fatalf("call result %v", result)
	}
	w, response, _ = serve(t, s, call{method: "resources/list"}.request())
	if w.Code != http.StatusNotFound || response.Error == nil || response.Error.Code != CodeMethodNotFound {
		t.Fatalf("unknown method: %d %s", w.Code, w.Body.String())
	}
	w, response, _ = serve(t, s, call{method: "tools/call", name: "missing"}.request())
	if w.Code != http.StatusBadRequest || response.Error == nil || response.Error.Code != CodeInvalidParams {
		t.Fatalf("unknown tool: %d %s", w.Code, w.Body.String())
	}
}

func TestToolErrorsAreExecutionErrors(t *testing.T) {
	s := testServer(t)
	body := func(arguments string) string {
		return `{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"name":"echo","arguments":` + arguments + `,"_meta":{"` + MetaProtocolVersion + `":"` + Version + `","` + MetaClientCapabilities + `":{}}}}`
	}
	for name, tc := range map[string]struct{ arguments, message string }{
		"missing required": {`{}`, "message is required"},
		"wrong type":       {`{"message":1}`, "message must be a string"},
		"unknown argument": {`{"message":"x","extra":true}`, "extra is not a known argument"},
		"out of range":     {`{"message":"x","count":9}`, "count must be at most 3"},
		"not integer":      {`{"message":"x","count":1.5}`, "count must be an integer"},
		"handler error":    {`{"message":"fail"}`, "restricted: the message may not fail"},
	} {
		t.Run(name, func(t *testing.T) {
			r := call{method: "tools/call", body: body(tc.arguments)}.request()
			r.Header.Set(HeaderName, "echo")
			w, response, obs := serve(t, s, r)
			result, _ := response.Result.(map[string]any)
			if w.Code != http.StatusOK || result["isError"] != true || obs.Outcome != OutcomeInvalid {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if text := result["content"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(text, tc.message) {
				t.Fatalf("message %q", text)
			}
		})
	}
}

func TestHeaderValueEncoding(t *testing.T) {
	for original, encoded := range map[string]string{
		"us-west1":           "us-west1",
		"Hello, 世界":          "=?base64?SGVsbG8sIOS4lueVjA==?=",
		" padded ":           "=?base64?IHBhZGRlZCA=?=",
		"line1\nline2":       "=?base64?bGluZTEKbGluZTI=?=",
		"=?base64?literal?=": "=?base64?PT9iYXNlNjQ/bGl0ZXJhbD89?=",
	} {
		if got := EncodeHeaderValue(original); got != encoded {
			t.Fatalf("encode %q: %q", original, got)
		}
		decoded, err := DecodeHeaderValue(encoded)
		if err != nil || decoded != original {
			t.Fatalf("decode %q: %q %v", encoded, decoded, err)
		}
	}
	if _, err := DecodeHeaderValue("bad\x7fvalue"); err == nil {
		t.Fatal("control character accepted")
	}
}

func TestRegistryRejectsBadTools(t *testing.T) {
	registry := &Registry{}
	handler := func(context.Context, Call) (Result, error) { return Result{}, nil }
	if err := registry.Add(Tool{Name: "has space", Handler: handler}); err == nil {
		t.Fatal("bad name accepted")
	}
	if err := registry.Add(Tool{Name: "ok"}); err == nil {
		t.Fatal("missing handler accepted")
	}
	if err := registry.Add(Tool{Name: "ok", Handler: handler}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Add(Tool{Name: "ok", Handler: handler}); err == nil {
		t.Fatal("duplicate accepted")
	}
	if tools := registry.Tools(); len(tools) != 1 || tools[0].InputSchema["type"] != "object" {
		t.Fatalf("tools %v", tools)
	}
}

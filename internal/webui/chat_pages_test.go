package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

type chatBackend struct {
	fakeBackend
	methods []string
	actors  []string
	params  []json.RawMessage
}

func (b *chatBackend) Query(_ context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	b.methods = append(b.methods, method)
	b.actors = append(b.actors, actor)
	if len(params) != 0 {
		b.params = append(b.params, params[0])
	}
	switch method {
	case "browserooms":
		return map[string]any{"items": []any{roomRecord("general", "General", "open", "")}, "next_cursor": ""}, nil
	case "browsedirectmessages":
		return map[string]any{"events": []any{}, "next_cursor": "next"}, nil
	default:
		return nil, nil
	}
}

func chatApp(t *testing.T, actor string) (*App, *chatBackend) {
	t.Helper()
	backend := &chatBackend{fakeBackend: fakeBackend{policy: policy.Defaults(roomOwner)}}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return actor, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return app, backend
}

func TestRoomsRedirectToChatPreservesPrefixAndQuery(t *testing.T) {
	app, _ := chatApp(t, roomOwner)
	req := httptest.NewRequest(http.MethodGet, "/rooms?cursor=older", nil)
	// Tenant routing normally strips the /r/<slug> prefix before webui sees
	// the URL. Keep the original request URI here to cover the redirect helper.
	req.RequestURI = "/r/demo/rooms?cursor=older"
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)
	if res.Code != http.StatusMovedPermanently || res.Header().Get("Location") != "/r/demo/chat?cursor=older" {
		t.Fatalf("redirect = %d %q", res.Code, res.Header().Get("Location"))
	}
}

func TestChatPageRendersGroupsAndGuestSignin(t *testing.T) {
	app, _ := chatApp(t, roomOwner)
	res := httptest.NewRecorder()
	app.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/chat", nil))
	body := res.Body.String()
	if res.Code != http.StatusOK || !strings.Contains(body, "<h1>Chat</h1>") || !strings.Contains(body, "Direct chats") || !strings.Contains(body, `href="/rooms/general"`) {
		t.Fatalf("signed chat page: %d %s", res.Code, body)
	}
	guest, _ := chatApp(t, "")
	res = httptest.NewRecorder()
	guest.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/chat", nil))
	body = res.Body.String()
	if res.Code != http.StatusOK || !strings.Contains(body, `href="/signin?next=%2Fchat">Sign in</a> to open your direct chats.`) || strings.Contains(body, "<direct-chats") || strings.Contains(body, "<direct-start") {
		t.Fatalf("guest chat page: %d %s", res.Code, body)
	}
}

func TestDirectChatPageDoesNotRenderCiphertextForGuest(t *testing.T) {
	app, backend := chatApp(t, "")
	peer := strings.Repeat("d", 64)
	res := httptest.NewRecorder()
	app.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/chat/dm/"+peer, nil))
	body := res.Body.String()
	if res.Code != http.StatusOK || !strings.Contains(body, "Private conversation") || !strings.Contains(body, `href="/signin?next=%2Fchat%2Fdm%2F`+peer) || strings.Contains(body, "<direct-thread") || strings.Contains(body, "ciphertext") {
		t.Fatalf("guest direct chat: %d %s", res.Code, body)
	}
	for _, method := range backend.methods {
		if method == "browsedirectmessages" {
			t.Fatal("guest direct chat queried encrypted messages")
		}
	}
}

func TestDirectMessagesEndpointRequiresActorAndForwardsCursor(t *testing.T) {
	guest, _ := chatApp(t, "")
	res := httptest.NewRecorder()
	guest.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/chat/events?cursor=abc", nil))
	if res.Code != http.StatusUnauthorized || res.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("guest events endpoint: %d %q", res.Code, res.Header().Get("Cache-Control"))
	}
	app, backend := chatApp(t, roomOwner)
	res = httptest.NewRecorder()
	app.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/chat/events?cursor=abc", nil))
	if res.Code != http.StatusOK || res.Header().Get("Cache-Control") != "private, no-store" || res.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("events endpoint: %d headers=%v body=%s", res.Code, res.Header(), res.Body.String())
	}
	if len(backend.methods) != 1 || backend.methods[0] != "browsedirectmessages" || backend.actors[0] != roomOwner {
		t.Fatalf("backend calls = %#v %#v", backend.methods, backend.actors)
	}
	var query map[string]any
	if err := json.Unmarshal(backend.params[0], &query); err != nil || query["cursor"] != "abc" || query["limit"] != float64(50) {
		t.Fatalf("query = %#v", query)
	}
}

func TestChatRouteRejectsUnsupportedMethodsAndPaths(t *testing.T) {
	app, _ := chatApp(t, roomOwner)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, "/chat/events", http.StatusMethodNotAllowed},
		{http.MethodGet, "/chat/events/extra", http.StatusNotFound},
		{http.MethodGet, "/chat/dm/not-a-pubkey", http.StatusNotFound},
	} {
		res := httptest.NewRecorder()
		app.ServeHTTP(res, httptest.NewRequest(tc.method, tc.path, nil))
		if res.Code != tc.status {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, res.Code, tc.status)
		}
	}
}

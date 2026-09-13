package tinyclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRemoteKeepsTenantPrefixInPagesAssetsAndRedirects(t *testing.T) {
	backend := &fakeBackend{policy: DefaultPolicy(strings.Repeat("a", 64))}
	app, err := New(backend, Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.StripPrefix("/r/team", app))
	defer upstream.Close()
	remote, err := NewRemote(RemoteOptions{BackendURL: upstream.URL + "/r/team", PublicURL: "http://relay.example/r/team"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/r/team/", "/r/team/account", "/r/team/manifest.webmanifest", "/r/team/scripts/bridge.js", "/r/team/rooms"} {
		got := httptest.NewRecorder()
		remote.ServeHTTP(got, httptest.NewRequest("GET", "http://relay.example"+path, nil))
		if path == "/r/team/rooms" {
			if got.Code != 301 || got.Header().Get("Location") != "/r/team/chat" {
				t.Fatalf("redirect: %d %v", got.Code, got.Header())
			}
			continue
		}
		if got.Code != 200 {
			t.Fatalf("prefix %s: %d", path, got.Code)
		}
		if path == "/r/team/" && !strings.Contains(got.Body.String(), `src="/r/team/scripts/bridge.js?`) {
			t.Fatal("page dropped tenant asset prefix")
		}
		if path == "/r/team/manifest.webmanifest" && !strings.Contains(got.Body.String(), `"start_url": "/r/team/"`) {
			t.Fatal("manifest dropped tenant prefix")
		}
	}
	response := httptest.NewRecorder()
	remote.ServeHTTP(response, httptest.NewRequest("GET", "http://relay.example/r/other/", nil))
	if response.Code != 404 {
		t.Fatalf("escaped tenant prefix: %d", response.Code)
	}
}

func TestRemoteProxiesWebSocketUpgrades(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		kind, body, err := conn.Read(r.Context())
		if err == nil {
			_ = conn.Write(r.Context(), kind, body)
		}
	}))
	defer upstream.Close()
	frontend := httptest.NewUnstartedServer(nil)
	public := "http://" + frontend.Listener.Addr().String()
	handler, err := NewRemote(RemoteOptions{BackendURL: upstream.URL, PublicURL: public})
	if err != nil {
		t.Fatal(err)
	}
	frontend.Config.Handler = handler
	frontend.Start()
	defer frontend.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, frontend.URL, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {public}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`["REQ","test",{}]`)); err != nil {
		t.Fatal(err)
	}
	_, body, err := conn.Read(ctx)
	if err != nil || string(body) != `["REQ","test",{}]` {
		t.Fatalf("websocket relay: %s %v", body, err)
	}
}

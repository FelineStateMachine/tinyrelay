package tinyclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestRemoteServesImmutableAssetsWithoutBackendReads(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "backend should not be called for immutable assets", http.StatusBadGateway)
	}))
	remote, err := NewRemote(RemoteOptions{BackendURL: upstream.URL, PublicURL: "http://relay.example"})
	if err != nil {
		t.Fatal(err)
	}
	upstream.Close()
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, path := range []string{"/scripts/bridge.js", "/signer.js", "/fixi.js", "/sw.js", "/icon.svg", "/icon-192.png"} {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(method, "http://relay.example"+path, nil)
			request.AddCookie(&http.Cookie{Name: "tiny_session", Value: "session"})
			remote.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK || calls.Load() != 0 {
				t.Fatalf("%s %s: status=%d backend calls=%d", method, path, recorder.Code, calls.Load())
			}
		}
	}
	for _, path := range []string{"/r/other/scripts/bridge.js", "/r/other/icon.svg"} {
		recorder := httptest.NewRecorder()
		remote.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://relay.example"+path, nil))
		if recorder.Code != http.StatusNotFound || calls.Load() != 0 {
			t.Fatalf("cross-tenant %s: status=%d backend calls=%d", path, recorder.Code, calls.Load())
		}
	}
}

func TestRemoteEmbeddedAssetsMatchIntegratedClient(t *testing.T) {
	app, err := New(&fakeBackend{policy: DefaultPolicy(strings.Repeat("a", 64))}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected upstream request", http.StatusBadGateway)
	}))
	defer upstream.Close()
	paths := []string{"/scripts/bridge.js", "/scripts/bridge.js?v=" + scriptsVersion, "/signer.js", "/fixi.js", "/sw.js", "/icon.svg", "/icon-mono.svg", "/icon-192.png"}
	for _, prefix := range []string{"", "/r/team"} {
		remote, err := NewRemote(RemoteOptions{BackendURL: upstream.URL + prefix, PublicURL: "http://relay.example" + prefix})
		if err != nil {
			t.Fatal(err)
		}
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			for _, path := range paths {
				request := httptest.NewRequest(method, "http://relay.example"+prefix+path, nil)
				request.AddCookie(&http.Cookie{Name: "tiny_session", Value: "session"})
				got, want := httptest.NewRecorder(), httptest.NewRecorder()
				remote.ServeHTTP(got, request)
				local := request.Clone(request.Context())
				local.URL.Path = strings.TrimPrefix(local.URL.Path, prefix)
				app.ServeHTTP(want, local)
				if got.Code != want.Code || got.Body.String() != want.Body.String() {
					t.Fatalf("%s %s%s: standalone status/body differs from integrated", method, prefix, path)
				}
				if method == http.MethodHead && got.Body.Len() != 0 {
					t.Fatalf("HEAD %s carries a response body", path)
				}
				for _, header := range []string{"Content-Type", "Cache-Control", "Service-Worker-Allowed"} {
					if got.Header().Get(header) != want.Header().Get(header) {
						t.Fatalf("%s %s: %s=%q, want %q", method, path, header, got.Header().Get(header), want.Header().Get(header))
					}
				}
			}
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("embedded assets made %d upstream requests", calls.Load())
	}
}

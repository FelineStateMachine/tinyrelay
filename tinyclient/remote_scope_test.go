package tinyclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRemoteRejectsCrossTenantRoutesBeforeRenderingOrProxying(t *testing.T) {
	app, err := New(&fakeBackend{policy: DefaultPolicy(strings.Repeat("a", 64))}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, BackendPath) {
			app.BackendHandler().ServeHTTP(w, r)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	for _, mount := range []string{"", "/r/team"} {
		t.Run("mount="+mount, func(t *testing.T) {
			remote, err := NewRemote(RemoteOptions{BackendURL: upstream.URL + mount, PublicURL: "http://relay.example" + mount})
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name, method, path, header, value string
			}{
				{"page", "GET", "/r/other/account", "", ""},
				{"head", "HEAD", "/r/other/account", "", ""},
				{"feed", "GET", "/r/other/articles.json", "", ""},
				{"asset", "GET", "/r/other/scripts/bridge.js", "", ""},
				{"tenant root", "GET", "/r/other", "", ""},
				{"tenant home", "GET", "/r/other/", "", ""},
				{"events", "POST", "/r/other/events", "", ""},
				{"session", "POST", "/r/other/session", "", ""},
				{"delete", "DELETE", "/r/other/events", "", ""},
				{"options", "OPTIONS", "/r/other/events", "", ""},
				{"signed page", "GET", "/r/other/account", "Authorization", "Nostr exact-proof"},
				{"websocket", "GET", "/r/other/", "Upgrade", "websocket"},
				{"nostr info", "GET", "/r/other/", "Accept", "application/nostr+json"},
				{"stream", "GET", "/r/other/rooms/build/stream", "", ""},
				{"adapter", "GET", "/r/other" + BackendPath, "", ""},
				{"encoded tenant prefix", "GET", "/%72/other/account", "", ""},
			} {
				t.Run(tc.name, func(t *testing.T) {
					req := httptest.NewRequest(tc.method, "http://relay.example"+tc.path, nil)
					req.AddCookie(&http.Cookie{Name: "tiny_session", Value: "root-session"})
					if tc.header != "" {
						req.Header.Set(tc.header, tc.value)
					}
					if tc.header == "Upgrade" {
						req.Header.Set("Connection", "Upgrade")
					}
					before := calls.Load()
					got := httptest.NewRecorder()
					remote.ServeHTTP(got, req)
					if got.Code != http.StatusNotFound || calls.Load() != before {
						t.Fatalf("cross-tenant request reached renderer or proxy: status=%d upstream calls=%d", got.Code, calls.Load()-before)
					}
				})
			}
			// The scope guard must not disable the configured tenant's routes.
			for _, tc := range []struct {
				method, path string
				status       int
			}{
				{"GET", "/account", 200},
				{"POST", "/events", 202},
				{"GET", "/rooms/build/stream", 202},
			} {
				got := httptest.NewRecorder()
				remote.ServeHTTP(got, httptest.NewRequest(tc.method, "http://relay.example"+mount+tc.path, nil))
				if got.Code != tc.status {
					t.Errorf("valid tenant %s %s: status=%d want=%d", tc.method, tc.path, got.Code, tc.status)
				}
			}
		})
	}
}

package tinyclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBackendSnapshotHidesOwnerSettings(t *testing.T) {
	p := DefaultPolicy(strings.Repeat("a", 64))
	p.Name = "Visible relay"
	p.PrivatePeers = []string{"https://secret-peer.example"}
	p.PushCallbacks = []string{"https://secret-hook.example/token"}
	p.BlockedWords = []string{"secret-moderation-rule"}
	app, err := New(&fakeBackend{policy: p}, Options{Actor: func(r *http.Request) (string, error) {
		c, err := r.Cookie("tiny_session")
		if err == nil && c.Value == "valid-owner" {
			return p.Owner, nil
		}
		return "", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []bool{false, true} {
		req := httptest.NewRequest("GET", BackendPath, nil)
		if owner {
			req.AddCookie(&http.Cookie{Name: "tiny_session", Value: "valid-owner"})
		}
		response := httptest.NewRecorder()
		app.BackendHandler().ServeHTTP(response, req)
		body := response.Body.String()
		if response.Code != 200 || !strings.Contains(body, "Visible relay") {
			t.Fatalf("snapshot: %d %s", response.Code, body)
		}
		for _, secret := range []string{"secret-peer", "secret-hook", "secret-moderation-rule"} {
			if strings.Contains(body, secret) != owner {
				t.Fatalf("owner=%v snapshot leaks or loses %s: %s", owner, secret, body)
			}
		}
	}
}

func TestIntegratedAppMountsBackendContract(t *testing.T) {
	app, err := New(&fakeBackend{policy: DefaultPolicy(strings.Repeat("a", 64))}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(app)
	defer upstream.Close()
	remote, err := NewRemote(RemoteOptions{BackendURL: upstream.URL, PublicURL: "http://relay.example"})
	if err != nil {
		t.Fatal(err)
	}
	got := httptest.NewRecorder()
	remote.ServeHTTP(got, httptest.NewRequest("GET", "http://relay.example/", nil))
	if got.Code != 200 || !strings.Contains(got.Body.String(), "<html") {
		t.Fatalf("integrated backend: %d %s", got.Code, got.Body.String())
	}
}

func TestRemoteRendersHomeLocally(t *testing.T) {
	backend := &fakeBackend{policy: DefaultPolicy(strings.Repeat("a", 64))}
	integrated, err := New(backend, Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != BackendPath {
			t.Errorf("remote asked upstream to render %s", r.URL.Path)
			http.Error(w, "no renderer here", 418)
			return
		}
		integrated.BackendHandler().ServeHTTP(w, r)
	}))
	defer upstream.Close()
	remote, err := NewRemote(RemoteOptions{BackendURL: upstream.URL, PublicURL: backend.URL()})
	if err != nil {
		t.Fatal(err)
	}
	want, got := httptest.NewRecorder(), httptest.NewRecorder()
	integrated.ServeHTTP(want, httptest.NewRequest("GET", "/", nil))
	remote.ServeHTTP(got, httptest.NewRequest("GET", backend.URL()+"/", nil))
	if got.Code != want.Code || got.Body.String() != want.Body.String() {
		t.Fatalf("local rendering differs: status %d want %d\n%s", got.Code, want.Code, got.Body.String())
	}
}

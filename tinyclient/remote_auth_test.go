package tinyclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRemoteRejectsWrongPublicHostBeforeForwarding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("forwarded request for a foreign public host") }))
	defer upstream.Close()
	remote, err := NewRemote(RemoteOptions{BackendURL: upstream.URL, PublicURL: "http://relay.example"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "http://attacker.example/session", nil)
	request.Header.Set("Authorization", "Nostr exact-proof")
	response := httptest.NewRecorder()
	remote.ServeHTTP(response, request)
	if response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("foreign host accepted: %d", response.Code)
	}
}

func TestBackendContractNeverTrustsCallerPubkeysOrWrites(t *testing.T) {
	owner := strings.Repeat("a", 64)
	backend := &fakeBackend{policy: DefaultPolicy(owner)}
	app, err := New(backend, Options{Actor: func(r *http.Request) (string, error) {
		cookie, err := r.Cookie("tiny_session")
		if err == nil && cookie.Value == "valid-session" {
			return owner, nil
		}
		return "", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, authenticated := range []bool{false, true} {
		req := httptest.NewRequest("GET", BackendPath+"?method=browserepos&actor="+owner, nil)
		req.Header.Set("X-Actor", owner)
		if authenticated {
			req.AddCookie(&http.Cookie{Name: "tiny_session", Value: "valid-session"})
		}
		response := httptest.NewRecorder()
		app.BackendHandler().ServeHTTP(response, req)
		want := "browserepos:"
		if authenticated {
			want += owner
		}
		if response.Code != 200 || backend.call != want {
			t.Fatalf("actor trusted incorrectly: %d call=%s", response.Code, backend.call)
		}
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", BackendPath + "?method=setpolicy", 400},
		{"POST", BackendPath + "?method=browserepos", 405},
		{"GET", BackendPath + "?method=browserepos&params=invalid", 400},
	} {
		backend.call = ""
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.AddCookie(&http.Cookie{Name: "tiny_session", Value: "valid-session"})
		response := httptest.NewRecorder()
		app.BackendHandler().ServeHTTP(response, req)
		if response.Code != tc.status || backend.call != "" {
			t.Fatalf("unsafe query: %d call=%s", response.Code, backend.call)
		}
	}
}

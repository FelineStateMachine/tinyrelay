package tinyclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRemoteProxiesRelayProtocolsWithoutChangingSignedRequest(t *testing.T) {
	const public = "http://client.example"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == BackendPath {
			http.Error(w, "unexpected renderer request", 500)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.Host != "client.example" || r.Header.Get("Origin") != public || r.Header.Get("Cookie") != "tiny_session=token" {
			t.Errorf("request identity changed: host=%s headers=%v", r.Host, r.Header)
		}
		w.Header().Set("Set-Cookie", "tiny_session=updated; Path=/; HttpOnly; SameSite=Strict")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(202)
		_, _ = io.WriteString(w, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization")+" "+string(body))
	}))
	defer upstream.Close()
	remote, err := NewRemote(RemoteOptions{BackendURL: upstream.URL, PublicURL: public})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path, auth, body string }{
		{"POST", "/session", "Nostr exact-proof", "signed body"},
		{"POST", "/manage/rpc", "Nostr exact-proof", "signed body"},
		{"POST", "/events", "Nostr exact-proof", "signed body"},
		{"POST", "/query", "Nostr exact-proof", "signed body"},
		{"POST", "/mcp", "Nostr exact-proof", "signed body"},
		{"GET", "/rooms/build/stream?cursor=a%2Fb", "", ""},
		{"GET", "/views/plot/hash.svg", "", ""},
		{"GET", "/repo/raw?ref=main&path=a%2Fb", "", ""},
		{"GET", "/files/raw?hash=123", "", ""},
		{"GET", "/npub1example/repo.git/info/refs?service=git-upload-pack", "", ""},
		{"GET", "/healthz", "", ""},
		{"GET", "/.well-known/nostr.json", "", ""},
		{"GET", "/" + strings.Repeat("a", 64) + ".png", "", ""},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, public+tc.path, strings.NewReader(tc.body))
			request.Header.Set("Authorization", tc.auth)
			request.Header.Set("Origin", public)
			request.Header.Set("Cookie", "tiny_session=token")
			response := httptest.NewRecorder()
			remote.ServeHTTP(response, request)
			want := tc.method + " " + tc.path + " " + tc.auth + " " + tc.body
			if response.Code != 202 || response.Body.String() != want || !strings.Contains(response.Header().Get("Set-Cookie"), "tiny_session=updated") {
				t.Fatalf("proxy changed request: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

package tinyclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type actorFeedBackend struct{ fakeBackend }

func (b *actorFeedBackend) Query(_ context.Context, method string, _ []json.RawMessage, actor string) (any, error) {
	if method != "queryevents" {
		return nil, nil
	}
	items := []any{map[string]any{"id": "public-article", "content": "Public article", "created_at": 1}}
	if actor != "" {
		items = append(items, map[string]any{"id": "session-only-article", "content": "Session-only article", "created_at": 1})
	}
	return items, nil
}

func TestRemotePublicFeedsRemainAnonymousWithSession(t *testing.T) {
	backend := &actorFeedBackend{fakeBackend{policy: DefaultPolicy(strings.Repeat("a", 64))}}
	app, err := New(backend, Options{Actor: func(r *http.Request) (string, error) {
		if cookie, err := r.Cookie("tiny_session"); err == nil && cookie.Value == "valid-session" {
			return backend.policy.Owner, nil
		}
		return "", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(app)
	defer upstream.Close()
	remote, err := NewRemote(RemoteOptions{BackendURL: upstream.URL, PublicURL: backend.URL()})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/articles.json", "/feed.xml", "/feed"} {
		t.Run(path, func(t *testing.T) {
			anonymous := httptest.NewRecorder()
			app.ServeHTTP(anonymous, httptest.NewRequest("GET", backend.URL()+path, nil))
			if anonymous.Code != 200 || !strings.Contains(anonymous.Body.String(), "public-article") || strings.Contains(anonymous.Body.String(), "session-only-article") {
				t.Fatalf("invalid anonymous feed: %d %s", anonymous.Code, anonymous.Body.String())
			}
			for _, authenticated := range []bool{false, true} {
				request := httptest.NewRequest("GET", backend.URL()+path, nil)
				if authenticated {
					request.AddCookie(&http.Cookie{Name: "tiny_session", Value: "valid-session"})
				}
				integrated, standalone := httptest.NewRecorder(), httptest.NewRecorder()
				app.ServeHTTP(integrated, request.Clone(request.Context()))
				remote.ServeHTTP(standalone, request)
				for name, got := range map[string]*httptest.ResponseRecorder{"integrated": integrated, "standalone": standalone} {
					if got.Code != anonymous.Code || got.Body.String() != anonymous.Body.String() {
						t.Errorf("%s authenticated=%v feed differs from anonymous: %d %s", name, authenticated, got.Code, got.Body.String())
					}
				}
			}
		})
	}
}

package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatPreferencesWritesBindAccountAndSignedBody(t *testing.T) {
	app, tenant := testTenant(t)
	owner := tenant.Policy().Owner
	if err := tenant.store.PutSetting(context.Background(), chatPreferenceKey(owner), chatPreferences{SharePresence: true}); err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("0", 63) + "2"
	read := httptest.NewRequest("GET", "http://relay.test/chat/preferences?actor="+owner, nil)
	signRequestWithSecret(t, read, "", other)
	result := httptest.NewRecorder()
	app.ServeHTTP(result, read)
	if result.Code != 200 || result.Body.String() != `{"share_presence":false}` {
		t.Fatalf("cross-account read: %d %s", result.Code, result.Body.String())
	}
	for _, test := range []struct {
		name, body string
		signed     bool
		code       int
	}{
		{"unsigned", `{"share_presence":false}`, false, 401},
		{"actor injection", `{"actor":"` + owner + `","share_presence":false}`, true, 400},
		{"trailing object", `{"share_presence":false}{}`, true, 400},
		{"invalid type", `{"share_presence":"true"}`, true, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "http://relay.test/chat/preferences", strings.NewReader(test.body))
			if test.signed {
				signRequestWithSecret(t, req, test.body, other)
			}
			res := httptest.NewRecorder()
			app.ServeHTTP(res, req)
			if res.Code != test.code {
				t.Fatalf("status %d: %s", res.Code, res.Body.String())
			}
		})
	}
	altered := httptest.NewRequest("POST", "http://relay.test/chat/preferences", strings.NewReader(`{"share_presence":true}`))
	signRequest(t, altered, `{"share_presence":true}`)
	altered.Body = io.NopCloser(strings.NewReader(`{"share_presence":false}`))
	rejected := httptest.NewRecorder()
	app.ServeHTTP(rejected, altered)
	if rejected.Code != http.StatusUnauthorized {
		t.Fatalf("altered signed body: %d", rejected.Code)
	}
	if !tenant.chatSharePresence(context.Background(), owner) {
		t.Fatal("another account or invalid signature changed owner preference")
	}
}

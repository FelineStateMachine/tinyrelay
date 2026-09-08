package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSignInNavigationAndAccount(t *testing.T) {
	for _, actor := range []string{"", strings.Repeat("a", 64)} {
		app, err := New(&fakeBackend{}, Options{Actor: func(*http.Request) (string, error) { return actor, nil }})
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/", "/signin", "/manage/connect"} {
			response := httptest.NewRecorder()
			app.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			body := response.Body.String()
			if actor == "" && !strings.Contains(body, `href="/signin">Sign in</a>`) {
				t.Fatalf("%s missing sign-in navigation", path)
			}
			if actor != "" && (!strings.Contains(body, `title="`+actor+`"`) || strings.Count(body, `id="session-logout"`) != 1) {
				t.Fatalf("%s missing account or unique sign-out control", path)
			}
			if path == "/signin" && actor == "" && (!strings.Contains(body, `id="nostrconnect"`) || strings.Contains(body, `method="setconnections"`)) {
				t.Fatal("sign-in page must show signer controls without connection settings")
			}
			if actor != "" && strings.Contains(body, `id="nostrconnect"`) {
				t.Fatalf("%s shows signer controls to a signed-in account", path)
			}
		}
	}
}

package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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
			if actor == "" {
				wantLink := `href="/signin?next=`
				if path == "/signin" {
					wantLink = `href="/signin"`
				}
				if !strings.Contains(body, wantLink) {
					t.Fatalf("%s missing sign-in navigation", path)
				}
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

func TestSigninURLPreservesTenantPathAndQuery(t *testing.T) {
	got := signinURL("/r/work", "/rooms/abc", url.Values{
		"event": {"deadbeef"},
		"q":     {"hello world"},
	})
	want := "/signin?next=%2Fr%2Fwork%2Frooms%2Fabc%3Fevent%3Ddeadbeef%26q%3Dhello%2Bworld"
	if got != want {
		t.Fatalf("signinURL() = %q, want %q", got, want)
	}
}

func TestSigninURLWithoutTenant(t *testing.T) {
	if got := signinURL("", "/wiki", url.Values{"d": {"A page"}}); got != "/signin?next=%2Fwiki%3Fd%3DA%2Bpage" {
		t.Fatalf("signinURL() = %q", got)
	}
}

func TestSigninURLDoesNotNestSignin(t *testing.T) {
	for _, base := range []string{"", "/r/work"} {
		if got := signinURL(base, "/signin", url.Values{"next": {"/rooms"}}); got != "/signin" {
			t.Fatalf("signinURL(%q) = %q, want /signin", base, got)
		}
	}
}

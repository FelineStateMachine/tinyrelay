package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

type profileBackend struct {
	fakeBackend
	params json.RawMessage
}

func (b *profileBackend) Query(_ context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	if method != "browseprofile" {
		return nil, nil
	}
	if actor == "" {
		return nil, errors.New("auth-required: sign in")
	}
	b.params = params[0]
	return map[string]any{"pubkey": actor, "event": map[string]any{"kind": 0, "created_at": 1788900000}, "profile": map[string]any{"name": "dami", "about": "keeps <cats>", "lud16": "dami@wallet.example", "banner": ""}, "relays": []any{"wss://relay.one", "wss://relay.two"}}, nil
}

func TestProfilePagePrefillsTheFormAndOffersItFromHome(t *testing.T) {
	backend := &profileBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return backend.policy.Owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/profile", nil))
	body := recorder.Body.String()
	for _, marker := range []string{
		`<profile-form pubkey="` + backend.policy.Owner + `" existing="{`,
		`&#34;name&#34;:&#34;dami&#34;`,
		`<input name="name" maxlength="64" value="dami"`,
		`<textarea name="about" maxlength="2000">keeps &lt;cats&gt;</textarea>`,
		`<input name="lud16" value="dami@wallet.example"`,
		"wss://relay.one\nwss://relay.two\n</textarea>",
		`<button>Sign and publish</button>`,
		"Current profile published <time",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("profile page missing %q", marker)
		}
	}
	if strings.Contains(body, "class=") {
		t.Fatal("profile page carries a class attribute")
	}
	if !strings.Contains(string(backend.params), `"pubkey":"`+backend.policy.Owner+`"`) {
		t.Fatalf("profile query params = %s", backend.params)
	}
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	home := recorder.Body.String()
	if !strings.Contains(home, `<li><a href="/profile">profile</a></li>`) || strings.Contains(home, `<a href="/manage/people">manage</a>`) {
		t.Fatal("home panel does not offer the profile action, or still offers manage")
	}
}

func TestProfilePageAsksGuestsToSignIn(t *testing.T) {
	backend := &profileBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/profile", nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "Sign in to edit your profile.") || strings.Contains(body, "<profile-form") {
		t.Fatalf("guest profile page: %d %s", recorder.Code, body)
	}
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if home := recorder.Body.String(); !strings.Contains(home, `<li><a href="/signin?next=%2F">sign in</a></li>`) || strings.Contains(home, `href="/profile"`) {
		t.Fatal("guest home panel should offer sign in, not the profile")
	}
}

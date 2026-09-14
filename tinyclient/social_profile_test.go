package tinyclient

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

func TestSocialProfileAuthorAcceptsHexAndNpub(t *testing.T) {
	hexKey := strings.Repeat("ab", 32)
	if got, err := socialProfileAuthor(hexKey); err != nil || got != hexKey {
		t.Fatalf("hex author = %q, %v", got, err)
	}
	npub := "npub1424242424242424242424242424242424242424242424242424qamrcaj"
	if got, err := socialProfileAuthor(npub); err != nil || got != strings.Repeat("a", 64) {
		t.Fatalf("npub author = %q, %v", got, err)
	}
}

func TestSocialProfileAuthorRejectsInvalidKeys(t *testing.T) {
	for _, value := range []string{"", "alice", strings.Repeat("g", 64), "npub1invalid"} {
		if got, err := socialProfileAuthor(value); err == nil || got != "" {
			t.Errorf("author %q accepted as %q", value, got)
		}
	}
}

type socialProfileTestBackend struct {
	fakeBackend
	queries    []map[string]any
	failMethod string
}

func (b *socialProfileTestBackend) Query(_ context.Context, method string, params []json.RawMessage, _ string) (any, error) {
	var query map[string]any
	_ = json.Unmarshal(params[0], &query)
	b.queries = append(b.queries, query)
	if method == b.failMethod {
		return nil, errors.New("restricted: profile access denied")
	}
	switch method {
	case "browseprofile":
		return map[string]any{"profile": map[string]any{"name": "River", "website": "https://river.example"}}, nil
	case "browsesocial":
		return map[string]any{"items": []any{map[string]any{"id": "post", "kind": 1, "pubkey": query["author"], "content": "From River"}}, "next_cursor": "next"}, nil
	default:
		return nil, nil
	}
}

func TestSocialProfilePageUsesActorAndAuthorScopedFeed(t *testing.T) {
	owner := strings.Repeat("a", 64)
	b := &socialProfileTestBackend{fakeBackend: fakeBackend{policy: policy.Defaults(owner)}}
	a, err := New(b, Options{Actor: func(*http.Request) (string, error) { return owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/social/profile?kind=notes", nil))
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, "River") || !strings.Contains(body, "From River") || !strings.Contains(body, "Older posts") || !strings.Contains(body, "/profile") {
		t.Fatalf("own profile: %d %s", w.Code, body)
	}
	if len(b.queries) != 2 || b.queries[0]["pubkey"] != owner || b.queries[1]["author"] != owner || b.queries[1]["kind"] != "notes" {
		t.Fatalf("profile queries: %#v", b.queries)
	}
}

func TestSocialProfilePageGuestCanViewOtherAndMustChooseOwn(t *testing.T) {
	other := strings.Repeat("b", 64)
	b := &socialProfileTestBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	a, err := New(b, Options{Actor: func(*http.Request) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/social/profile?author="+other, nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "From River") || strings.Contains(w.Body.String(), "Edit profile") {
		t.Fatalf("guest profile: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/social/profile", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Sign in") || !strings.Contains(w.Body.String(), "View profile") {
		t.Fatalf("guest profile chooser: %d %s", w.Code, w.Body.String())
	}
}

func TestSocialProfileErrorsDoNotRenderProfileOrFeed(t *testing.T) {
	for _, method := range []string{"browseprofile", "browsesocial"} {
		t.Run(method, func(t *testing.T) {
			key := strings.Repeat("a", 64)
			b := &socialProfileTestBackend{fakeBackend: fakeBackend{policy: policy.Defaults(key)}, failMethod: method}
			a, err := New(b, Options{})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/social/profile?author="+key, nil))
			body := w.Body.String()
			if w.Code != http.StatusForbidden || !strings.Contains(body, "profile access denied") || strings.Contains(body, `id="social-profile-head"`) || strings.Contains(body, "No posts yet") {
				t.Fatalf("failed profile response: %d %s", w.Code, body)
			}
		})
	}
}

func TestSocialProfileOtherAuthorKeepsPaginationAndHidesEditing(t *testing.T) {
	owner, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	b := &socialProfileTestBackend{fakeBackend: fakeBackend{policy: policy.Defaults(owner)}}
	a, err := New(b, Options{Actor: func(*http.Request) (string, error) { return owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/social/profile?author="+other+"&kind=photos&q=river&cursor=old", nil))
	body := w.Body.String()
	if w.Code != http.StatusOK || strings.Contains(body, "Edit profile") || !strings.Contains(body, `/social/profile?author=`+other+`&amp;cursor=next&amp;kind=photos&amp;q=river`) {
		t.Fatalf("other profile pagination: %d %s", w.Code, body)
	}
	if b.queries[1]["author"] != other || b.queries[1]["cursor"] != "old" || b.queries[1]["kind"] != "photos" {
		t.Fatalf("author-scoped page: %#v", b.queries)
	}
}

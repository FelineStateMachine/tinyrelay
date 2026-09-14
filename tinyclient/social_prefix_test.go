package tinyclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

func prefixedSocialRequest(path, query string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path+"?"+query, nil)
	r.RequestURI = "/r/team" + path + "?" + query
	return r
}

func TestSocialLinksKeepTenantPrefix(t *testing.T) {
	a, err := New(&socialBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, prefixedSocialRequest("/social", "kind=podcasts&author="+strings.Repeat("a", 64)))
	body := w.Body.String()
	for _, want := range []string{
		`href="/r/team/social"`,
		`href="/r/team/social?kind=photos"`,
		`href="/r/team/social/profile"`,
		`href="/r/team/social/profile?author=`,
		`href="/r/team/social/podcasts.rss?author=`,
		`href="/r/team/social?author=`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing tenant-prefixed link %q in %s", want, body)
		}
	}
	if strings.Contains(body, `href="/social`) {
		t.Fatalf("unscoped Social link in %s", body)
	}
}

func TestSocialProfilePaginationKeepsTenantPrefix(t *testing.T) {
	owner, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	b := &socialProfileTestBackend{fakeBackend: fakeBackend{policy: policy.Defaults(owner)}}
	a, err := New(b, Options{Actor: func(*http.Request) (string, error) { return owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, prefixedSocialRequest("/social/profile", "author="+other+"&kind=photos"))
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `/r/team/social/profile?author=`+other+`&amp;cursor=next&amp;kind=photos`) {
		t.Fatalf("profile pagination lost tenant prefix: %d %s", w.Code, body)
	}
	if strings.Contains(body, `href="/social`) {
		t.Fatalf("unscoped profile link in %s", body)
	}
}

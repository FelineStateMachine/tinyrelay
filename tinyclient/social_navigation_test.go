package tinyclient

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

func TestSocialNavigationSharesOneRail(t *testing.T) {
	for _, kind := range []string{"feed", "notes", "posts", "photos", "videos", "podcasts"} {
		t.Run(kind, func(t *testing.T) {
			b := &socialBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
			a, err := New(b, Options{})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRecorder()
			a.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/social?kind="+kind, nil))
			body := r.Body.String()
			for _, marker := range []string{`aria-label="Social"`, ">/feed</a>", ">/notes</a>", ">/posts</a>", ">/photos</a>", ">/videos</a>", ">/podcasts</a>", `href="/social/profile"`, `href="/social/profile?author=`} {
				if !strings.Contains(body, marker) {
					t.Errorf("missing %q", marker)
				}
			}
			if r.Code != http.StatusOK || strings.Contains(body, "class=") || strings.Contains(body, "\u00b7") {
				t.Fatalf("invalid social page: status=%d", r.Code)
			}
			if !strings.Contains(body, `aria-current="page">/`+kind+`</a>`) {
				t.Fatal("selected social view is missing")
			}
		})
	}
}

func TestSocialViewAliasesAndMobileTitle(t *testing.T) {
	for _, query := range []url.Values{{"kind": {"articles"}}, {"kinds": {"30023"}}} {
		view := socialView(query)
		if view.Key != "posts" || view.Label != "Posts" {
			t.Fatalf("article alias = %+v", view)
		}
	}
	header := mobileHeader(PageData{Tab: "social", Path: "/social", Query: url.Values{"kind": {"photos"}}, Slug: "tiny"})
	if header.Title != "Photos" || header.Subtitle != "Social" || header.BackURL != "/social" {
		t.Fatalf("category mobile header = %+v", header)
	}
}

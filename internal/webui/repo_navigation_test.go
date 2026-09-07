package webui

import (
	"net/url"
	"testing"
)

func TestRepositoryNavigationKeepsSelectedRef(t *testing.T) {
	query := url.Values{"owner": {"alice"}, "repo": {"notes"}, "ref": {"refs/heads/feature/x"}, "path": {"src/lib/main.go"}, "offset": {"50"}}
	crumbs := repoBreadcrumbs(query)
	if len(crumbs) != 4 || crumbs[2].Label != "lib" {
		t.Fatalf("breadcrumbs = %#v", crumbs)
	}
	for _, crumb := range crumbs[:3] {
		link, err := url.Parse(crumb.URL)
		if err != nil || link.Query().Get("ref") != query.Get("ref") || link.Query().Get("offset") != "" {
			t.Fatalf("breadcrumb lost ref or kept page offset: %#v", crumb)
		}
	}
	link, _ := url.Parse(repoURL(query, "history", ""))
	if link.Query().Get("ref") != query.Get("ref") || link.Query().Get("path") != "" {
		t.Fatal("history navigation changed selected ref")
	}
}

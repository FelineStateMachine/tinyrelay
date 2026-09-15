package tinyclient

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

type repoCrumb struct{ Label, URL string }

// repoHandle is the owner segment for links from the current page: the
// resolved handle when the page carries one, else the owner as given.
func repoHandle(query url.Values) string {
	if handle := query.Get("handle"); handle != "" {
		return handle
	}
	return query.Get("owner")
}

// repoURL links a view of the current repository, keeping the branch and
// nothing else from the current query.
func repoURL(query url.Values, view, item string) string {
	keep := url.Values{}
	if ref := query.Get("ref"); ref != "" && view != "commit" {
		keep.Set("ref", ref)
	}
	return repoPath(repoHandle(query), query.Get("repo"), view, item, keep)
}

func repoCommitURL(query url.Values, oid string) string {
	return repoPath(repoHandle(query), query.Get("repo"), "commit", oid, nil)
}

// repoPagePath is the current repository page without its query, the
// action for forms that rebuild the query from their fields.
func repoPagePath(query url.Values) string {
	item := query.Get("path")
	if view := repoView(query); view == "issue" || view == "pr" || view == "commit" {
		item = query.Get("id")
	}
	return repoPath(repoHandle(query), query.Get("repo"), repoView(query), item, nil)
}

// repoPageURL is the current repository page with one query value changed.
func repoPageURL(query url.Values, field string, value any) string {
	values := transientQuery(query)
	values.Set(field, fmt.Sprint(value))
	item := query.Get("path")
	if view := repoView(query); view == "issue" || view == "pr" || view == "commit" {
		item = query.Get("id")
	}
	return repoPath(repoHandle(query), query.Get("repo"), repoView(query), item, values)
}

// repoHref links a repository by its owner handle, for lists.
func repoHref(handle, repo, view, item string) string {
	return repoPath(handle, repo, view, item, nil)
}

func hasNextOffset(value any) bool {
	switch offset := value.(type) {
	case int:
		return offset >= 0
	case float64:
		return offset >= 0
	default:
		return false
	}
}

func repoBreadcrumbs(query url.Values) []repoCrumb {
	crumbs := []repoCrumb{{Label: query.Get("repo"), URL: repoURL(query, "tree", "")}}
	if query.Get("path") == "" {
		return crumbs
	}
	parts := strings.Split(query.Get("path"), "/")
	for index, label := range parts {
		crumbs = append(crumbs, repoCrumb{Label: label, URL: repoURL(query, "tree", strings.Join(parts[:index+1], "/"))})
	}
	crumbs[len(crumbs)-1].URL = ""
	return crumbs
}

// browsePageURL is a list page with one query value changed. Values the
// page derives from its path are not written back.
func browsePageURL(route string, query url.Values, field string, value any) string {
	values := transientQuery(query)
	values.Set(field, fmt.Sprint(value))
	if value == "" || value == nil {
		values.Del(field)
	}
	if len(values) == 0 {
		return route
	}
	return route + "?" + values.Encode()
}

func shortID(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	if len(text) > 12 {
		return text[:12]
	}
	return text
}

func repoDate(value any) string {
	var seconds int64
	switch value := value.(type) {
	case float64:
		seconds = int64(value)
	case int64:
		seconds = value
	}
	if seconds == 0 {
		return ""
	}
	return time.Unix(seconds, 0).UTC().Format("Jan. 2, 2006")
}

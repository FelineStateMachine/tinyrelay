package webui

import (
	_ "embed"
	"fmt"
	"net/url"
	"strings"
	"time"
)

//go:embed repo.html
var repoTemplate string

type repoCrumb struct{ Label, URL string }

func repoURL(query url.Values, view, path string) string {
	values := url.Values{"owner": {query.Get("owner")}, "repo": {query.Get("repo")}, "ref": {query.Get("ref")}, "view": {view}}
	if path != "" {
		values.Set("path", path)
	}
	return "/repo?" + values.Encode()
}

func repoCommitURL(query url.Values, oid string) string {
	values := url.Values{"owner": {query.Get("owner")}, "repo": {query.Get("repo")}, "ref": {oid}}
	return repoURL(values, "commit", "")
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

func browsePageURL(route string, query url.Values, field string, value any) string {
	values := make(url.Values, len(query)+1)
	for name, items := range query {
		values[name] = append([]string(nil), items...)
	}
	values.Set(field, fmt.Sprint(value))
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

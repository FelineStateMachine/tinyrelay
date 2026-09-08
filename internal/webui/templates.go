package webui

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/url"
	"strings"
	"time"
)

// templateFS holds every page template. page.html owns the shell and the
// shared partials; the other files each define one group of tabs.
//
//go:embed page.html public.html manage.html browse.html repo.html
var templateFS embed.FS

// styleCSS is inlined into every page so the UI needs no extra request and
// works unchanged under a tenant path prefix. It styles ids, elements and the
// custom element names only; templates never carry class attributes.
//
//go:embed style.css
var styleCSS string

// bridgeJS is the signer bridge and componentsJS defines the custom
// elements. Both are inlined at the end of the body in that order.
//
//go:embed bridge.js
var bridgeJS string

//go:embed components.js
var componentsJS string

// Installable app assets. The manifest is a template: NAME and BASE are
// replaced per request so tenant prefixes and relay names stay correct.
//
//go:embed manifest.webmanifest
var manifestTemplate string

//go:embed sw.js
var serviceWorkerJS []byte

//go:embed icon.svg
var iconSVG []byte

//go:embed icon-mono.svg
var iconMonoSVG []byte

// navItem is one rail entry. Tab matches PageData.Tab for the active state.
type navItem struct{ Label, Href, Tab string }

var relayNav = []navItem{{"/home", "/", "home"}, {"/search", "/search", "search"}, {"/repos", "/repos", "repos"}, {"/files", "/files", "files"}, {"/sites", "/sites", "sites"}, {"/inbox", "/inbox", "inbox"}, {"/outbox", "/outbox", "outbox"}, {"/articles", "/articles", "articles"}, {"/manage", "/manage/people", "manage"}}

var manageNav = []navItem{{"/people", "/manage/people", "people"}, {"/moderation", "/manage/moderation", "moderation"}, {"/rules", "/manage/rules", "rules"}, {"/identity", "/manage/identity", "identity"}, {"/connect", "/manage/connect", "connect"}, {"/data", "/manage/data", "data"}, {"/sync", "/manage/sync", "sync"}, {"/views", "/manage/views", "views"}, {"/health", "/manage/health", "health"}, {"/owner", "/manage/owner", "owner"}, {"/status", "/manage/status", "status"}, {"/tools", "/tools", "tools"}}

// railKind picks the sitemap for a tab: one repository, management, or the relay.
func railKind(tab string) string {
	if tab == "repo" {
		return "repo"
	}
	for _, item := range manageNav {
		if item.Tab == tab {
			return "manage"
		}
	}
	return "relay"
}

// promptPath renders the footer prompt: the path after the relay name, with
// repository pages spelled out as repos/<name>/<view>.
func promptPath(path string, query url.Values) string {
	path = strings.Trim(path, "/")
	switch {
	case path == "":
		return "home"
	case path == "repo" && query.Get("repo") != "":
		return "repos/" + query.Get("repo") + "/" + repoView(query)
	case path == "file" && (query.Get("sha") != "" || query.Get("hash") != ""):
		return "files/" + shortID(query.Get("sha")+query.Get("hash"))
	}
	return path
}

// short renders a Unix timestamp as a compact UTC stamp for list rows.
func short(value any) string {
	seconds := unixSeconds(value)
	if seconds <= 0 {
		return ""
	}
	return time.Unix(seconds, 0).UTC().Format("Jan 2 15:04")
}

func repoView(query url.Values) string {
	if view := query.Get("view"); view != "" {
		return view
	}
	return "tree"
}

func parseTemplates() (*template.Template, error) {
	scripts := map[string]string{"bridge.js": bridgeJS, "components.js": componentsJS}
	funcs := template.FuncMap{
		"stylesheet": func() template.CSS { return template.CSS(styleCSS) },
		"script": func(name string) (template.JS, error) {
			source, ok := scripts[name]
			if !ok {
				return "", fmt.Errorf("unknown script %q", name)
			}
			return template.JS(source), nil
		},
		"json": func(value any) string {
			encoded, err := json.Marshal(value)
			if err != nil {
				return "null"
			}
			return string(encoded)
		},
		"join":            strings.Join,
		"urlquery":        url.QueryEscape,
		"asMap":           valueMap,
		"str":             plainString,
		"datetime":        datetime,
		"when":            when,
		"markdown":        renderMarkdown,
		"hasPrefix":       strings.HasPrefix,
		"npub":            identityNpub,
		"short":           short,
		"prompt":          promptPath,
		"railKind":        railKind,
		"repoView":        repoView,
		"relayItems":      func() []navItem { return relayNav },
		"manageItems":     func() []navItem { return manageNav },
		"add":             func(a, b int) int { return a + b },
		"repoCommitURL":   repoCommitURL,
		"hasNextOffset":   hasNextOffset,
		"repoURL":         repoURL,
		"repoBreadcrumbs": repoBreadcrumbs,
		"browsePageURL":   browsePageURL,
		"shortID":         shortID,
		"repoDate":        repoDate,
		"sourceHTML":      sourceHTML,
		"diffHTML":        diffHTML,
	}
	tmpl, err := template.New("webui").Funcs(funcs).ParseFS(templateFS, "*.html")
	if err != nil {
		return nil, fmt.Errorf("parse web templates: %w", err)
	}
	return tmpl, nil
}

func valueMap(value any) map[string]any {
	if result, ok := value.(map[string]any); ok {
		return result
	}
	encoded, _ := json.Marshal(value)
	var result map[string]any
	_ = json.Unmarshal(encoded, &result)
	return result
}

// plainString prints a decoded JSON value for display: nil becomes empty,
// whole floats print without a fraction, and everything else uses fmt.
func plainString(value any) string {
	switch value := value.(type) {
	case nil:
		return ""
	case string:
		return value
	case float64:
		if value == float64(int64(value)) {
			return fmt.Sprintf("%d", int64(value))
		}
		return fmt.Sprintf("%v", value)
	default:
		return fmt.Sprint(value)
	}
}

// datetime renders a Unix timestamp as UTC text suitable for a time element.
func datetime(value any) string {
	seconds := unixSeconds(value)
	if seconds <= 0 {
		return ""
	}
	return time.Unix(seconds, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// when renders a Unix timestamp as short UTC text for people to read.
func when(value any) string {
	seconds := unixSeconds(value)
	if seconds <= 0 {
		return ""
	}
	return time.Unix(seconds, 0).UTC().Format("2006-01-02 15:04 UTC")
}

func unixSeconds(value any) int64 {
	switch value := value.(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	}
	return 0
}

// tableRows renders escaped table head and body markup for fragments and
// streams that swap into an existing table element.
func tableRows(headers []string, rows [][]string) string {
	var out strings.Builder
	out.WriteString(`<thead><tr>`)
	for _, header := range headers {
		out.WriteString(`<th scope="col">` + template.HTMLEscapeString(header) + `</th>`)
	}
	out.WriteString(`</tr></thead><tbody>`)
	if len(rows) == 0 {
		out.WriteString(`<tr><td colspan="` + fmt.Sprint(len(headers)) + `">No entries.</td></tr>`)
	}
	for _, row := range rows {
		out.WriteString(`<tr>`)
		for _, cell := range row {
			out.WriteString(`<td>` + template.HTMLEscapeString(cell) + `</td>`)
		}
		out.WriteString(`</tr>`)
	}
	out.WriteString(`</tbody>`)
	return out.String()
}

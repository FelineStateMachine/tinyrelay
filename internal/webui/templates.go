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
		"join":     strings.Join,
		"urlquery": url.QueryEscape,
		"asMap":    valueMap,
		"str":      plainString,
		"datetime": datetime,
		"when":     when,
		"selectedView": func(query url.Values, view string) string {
			if query.Get("view") == view || (query.Get("view") == "" && view == "tree") {
				return " selected"
			}
			return ""
		},
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

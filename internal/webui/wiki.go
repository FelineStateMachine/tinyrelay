package webui

// Wiki pages: the view model derived from the browsewikipage result so the
// templates read plain fields, and the URL helpers for page names.

import (
	"html/template"
	"net/url"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/wiki"
)

// wikiView is one page as the templates see it. Version is the version
// being shown, Number its place counted from the oldest, Others the rest.
type wikiView struct {
	D, Title      string
	Version       map[string]any
	Author        string
	Number, Count int
	Forks, Links  int
	Versions      []map[string]any
	Others        []map[string]any
	Merges        []map[string]any
	OpenMerges    []map[string]any
	OpenCount     int
	RedirectsTo   []map[string]any
	RedirectsFrom []map[string]any
}

func wikiMaps(value any) []map[string]any {
	items, _ := value.([]any)
	maps := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m := valueMap(item); m != nil {
			maps = append(maps, m)
		}
	}
	return maps
}

// wikiPageView reads the browsewikipage result. Open merge requests aimed at
// the shown version's author come first in OpenMerges; OpenCount counts every
// open request on the page.
func wikiPageView(result any) wikiView {
	page := valueMap(result)
	view := wikiView{D: plainString(page["d"]), Title: plainString(page["title"])}
	view.Versions = wikiMaps(page["versions"])
	view.Merges = wikiMaps(page["merges"])
	view.RedirectsTo = wikiMaps(page["redirects_to"])
	view.RedirectsFrom = wikiMaps(page["redirects_from"])
	view.Count = len(view.Versions)
	if current := valueMap(page["version"]); current != nil {
		view.Version = current
		view.Author = plainString(current["author"])
		if links, ok := current["links"].([]any); ok {
			view.Links = len(links)
		}
	}
	if view.Title == "" {
		view.Title = view.D
	}
	id, coordinate := plainString(view.Version["id"]), plainString(view.Version["coordinate"])
	for i, v := range view.Versions {
		if plainString(v["id"]) == id {
			view.Number = view.Count - i
			continue
		}
		view.Others = append(view.Others, v)
		fork := valueMap(v["fork"])
		if fork != nil && (plainString(fork["e"]) == id || (coordinate != "" && plainString(fork["a"]) == coordinate)) {
			view.Forks++
		}
	}
	for _, m := range view.Merges {
		if plainString(m["status"]) != "open" {
			continue
		}
		view.OpenCount++
		if plainString(m["destination"]) == view.Author {
			view.OpenMerges = append(view.OpenMerges, m)
		}
	}
	return view
}

// wikiURL builds the page path for a name, with optional query pairs.
func wikiURL(d string, pairs ...string) string {
	path := "/wiki/" + url.PathEscape(d)
	values := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			values.Set(pairs[i], pairs[i+1])
		}
	}
	if len(values) == 0 {
		return path
	}
	return path + "?" + values.Encode()
}

func wikiHTML(content any) template.HTML {
	return wiki.RenderHTML(plainString(content))
}

// wikiPageName is the name in a /wiki/<d> path, normalized.
func wikiPageName(path string) string {
	name, err := url.PathUnescape(strings.TrimPrefix(path, "/wiki/"))
	if err != nil {
		name = strings.TrimPrefix(path, "/wiki/")
	}
	return wiki.Normalize(name)
}

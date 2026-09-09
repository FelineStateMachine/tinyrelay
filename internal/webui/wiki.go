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
// Revision and Revisions number the shown version among its author's
// revisions; History lists every revision the viewer may see, newest
// first. Archived is set when an older revision was opened by id, and
// ReadersRevision names the approved revision readers see while the
// shown one is a proposal that is not approved.
type wikiView struct {
	D, Title            string
	Version             map[string]any
	Author              string
	Number, Count       int
	Revision, Revisions int
	Forks, Links        int
	Versions            []map[string]any
	Others              []map[string]any
	History             []map[string]any
	Archived            bool
	ReadersRevision     int
	Merges              []map[string]any
	OpenMerges          []map[string]any
	OpenCount           int
	RedirectsTo         []map[string]any
	RedirectsFrom       []map[string]any
	// CanApprove is set when the viewer may accept or reject proposals.
	CanApprove bool
}

func wikiInt(value any) int {
	number, _ := value.(float64)
	return int(number)
}

// wikiProposalView is how a version's proposal state reads for one viewer.
// Shown is set for the people who may see the state: the owner and
// moderators, and the version's author. Decide is set when the viewer may
// accept or reject it, which is only while it is pending.
type wikiProposalView struct {
	Shown, Decide bool
	State         string
	At            any
	By            string
}

// wikiProposal reads a version's proposal state for the viewer. canApprove
// is the browse result's can_approve flag.
func wikiProposal(version map[string]any, actor string, canApprove any) wikiProposalView {
	if version == nil || version["proposal"] != true {
		return wikiProposalView{}
	}
	approver, _ := canApprove.(bool)
	if !approver && (actor == "" || actor != plainString(version["author"])) {
		return wikiProposalView{}
	}
	view := wikiProposalView{Shown: true, State: plainString(version["approval"]), At: version["approval_at"], By: plainString(version["approval_by"])}
	view.Decide = approver && view.State == "pending"
	return view
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
	view.History = wikiMaps(page["history"])
	view.CanApprove, _ = page["can_approve"].(bool)
	view.Count = len(view.Versions)
	if current := valueMap(page["version"]); current != nil {
		view.Version = current
		view.Author = plainString(current["author"])
		view.Revision, view.Revisions = wikiInt(current["revision"]), wikiInt(current["revisions"])
		view.Archived = plainString(current["superseded_by"]) != "" && plainString(page["preferred_by"]) == "version"
		if links, ok := current["links"].([]any); ok {
			view.Links = len(links)
		}
		if current["proposal"] == true && plainString(current["approval"]) != "approved" {
			for _, h := range view.History {
				if plainString(h["author"]) == view.Author && plainString(h["approval"]) == "approved" {
					view.ReadersRevision = wikiInt(h["revision"])
					break
				}
			}
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

func (a *App) wikiHTML(content any) template.HTML {
	return wiki.RenderHTMLWith(plainString(content), wiki.Options{Block: a.blocks()})
}

// wikiPageName is the name in a /wiki/<d> path, normalized.
func wikiPageName(path string) string {
	name, err := url.PathUnescape(strings.TrimPrefix(path, "/wiki/"))
	if err != nil {
		name = strings.TrimPrefix(path, "/wiki/")
	}
	return wiki.Normalize(name)
}

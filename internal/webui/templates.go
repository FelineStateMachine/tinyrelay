package webui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/url"
	"sort"
	"strings"
	"time"
)

// templateFS holds every page template. page.html owns the shell and the
// shared partials; the other files each define one group of tabs.
//
//go:embed page.html public.html manage.html browse.html repo.html collaboration.html wiki.html rooms.html social.html
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

//go:embed tiny.js
var tinyJS string

//go:embed components.js
var componentsJS string

//go:embed rooms.js
var roomsJS string

// nostr-name.js is the vendored <nostr-name> element with the relay-local
// profile loader; rebuild it with npm run build:nostr-name.
//
//go:embed nostr-name.js
var nostrNameJS string

//go:embed blossom-encryption.js
var blossomEncryptionJS string

//go:embed blossom-manifests.js
var blossomManifestsJS string

//go:embed file-messages.js
var fileMessagesJS string

//go:embed private-services.js
var privateServicesJS string

//go:embed blossom-upload.js
var blossomUploadJS string

//go:embed file-workspace.js
var fileWorkspaceJS string

//go:embed file-catalog.js
var fileCatalogJS string

// Installable app assets. The manifest is a template: NAME and BASE are
// replaced per request so tenant prefixes and relay names stay correct.
//
//go:embed manifest.webmanifest
var manifestTemplate string

//go:embed icon-192.png
var icon192PNG []byte

//go:embed icon-512.png
var icon512PNG []byte

//go:embed icon-maskable-512.png
var iconMaskablePNG []byte

//go:embed apple-touch-icon.png
var appleTouchIconPNG []byte

//go:embed badge-96.png
var badgePNG []byte

//go:embed screenshot-narrow.png
var screenshotNarrowPNG []byte

//go:embed screenshot-wide.png
var screenshotWidePNG []byte

//go:embed sw.js
var serviceWorkerJS []byte

//go:embed icon.svg
var iconSVG []byte

//go:embed icon-mono.svg
var iconMonoSVG []byte

// navItem is one rail entry. Tab matches PageData.Tab for the active state.
// navItem is one rail entry. Group orders entries by what they are for; the
// rail leaves a small gap between groups.
type navItem struct {
	Label, Href, Tab string
	Group            int
}

// relayNav: entry points, then conversation, then collaborative artifacts,
// then what the relay publishes and stores, then what is yours.
var relayNav = []navItem{{"/home", "/", "home", 0}, {"/search", "/search", "search", 0}, {"/rooms", "/rooms", "rooms", 1}, {"/inbox", "/inbox", "inbox", 1}, {"/approvals", "/approvals", "approvals", 1}, {"/repos", "/repos", "repos", 2}, {"/wiki", "/wiki", "wiki", 2}, {"/files", "/files", "files", 3}, {"/social", "/social", "social", 3}, {"/sites", "/sites", "sites", 3}, {"/outbox", "/outbox", "outbox", 4}, {"/manage", "/manage/people", "manage", 4}}

// manageNav: who is here, what they may do, what the relay is, what it does
// over time, and how it is doing.
var manageNav = []navItem{{"/people", "/manage/people", "people", 0}, {"/agents", "/manage/agents", "agents", 0}, {"/moderation", "/manage/moderation", "moderation", 1}, {"/rules", "/manage/rules", "rules", 1}, {"/identity", "/manage/identity", "identity", 2}, {"/connect", "/manage/connect", "connect", 2}, {"/owner", "/manage/owner", "owner", 2}, {"/sync", "/manage/sync", "sync", 3}, {"/data", "/manage/data", "data", 3}, {"/views", "/manage/views", "views", 3}, {"/health", "/manage/health", "health", 4}}

// navGroups splits a rail list into its groups, in order.
func navGroups(items []navItem) [][]navItem {
	var groups [][]navItem
	for _, item := range items {
		if len(groups) == 0 || groups[len(groups)-1][0].Group != item.Group {
			groups = append(groups, nil)
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], item)
	}
	return groups
}

// railKind picks the sitemap for a tab: one repository, the rooms,
// management, or the relay.
func railKind(tab string) string {
	if tab == "repo" {
		return "repo"
	}
	if tab == "rooms" || tab == "room" || tab == "thread" {
		return "rooms"
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
	if room := roomRoute("/" + path); room.tab == "thread" {
		return "rooms/" + room.id + "/thread/" + shortID(room.event)
	}
	if strings.HasPrefix(path, "wiki/") {
		return "wiki/" + wikiPageName("/"+path)
	}
	return path
}

// pageAddress is the address the crumb copies: the relay's public URL plus
// the page's canonical path, so a link copied on a tailnet or LAN address
// still opens for anyone. Wiki names are normalized the way the relay
// resolves them; repositories and files keep the query that names them.
func pageAddress(base, path string, query url.Values) string {
	path = "/" + strings.Trim(path, "/")
	if path == "/" {
		path = ""
	}
	if strings.HasPrefix(path, "/wiki/") {
		path = "/wiki/" + url.PathEscape(wikiPageName(path))
	}
	keep := url.Values{}
	for _, key := range []string{"owner", "repo", "view", "path", "ref", "sha", "hash", "q", "id", "author", "version", "merge", "address", "kind"} {
		if value := query.Get(key); value != "" {
			keep.Set(key, value)
		}
	}
	address := strings.TrimSuffix(base, "/") + path
	if len(keep) > 0 {
		address += "?" + keep.Encode()
	}
	return address
}

// wsURL turns the relay's public http(s) URL into its websocket form.
func wsURL(base string) string {
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://")
	}
	return base
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

// scripts are served at /scripts/<name> and cached by the service worker.
// One version stamp covers them all, so a change to any file refreshes every
// cached copy together.
var scripts = map[string]string{"bridge.js": bridgeJS, "tiny.js": tinyJS, "components.js": componentsJS, "rooms.js": roomsJS, "nostr-name.js": nostrNameJS, "blossom-encryption.js": blossomEncryptionJS, "blossom-manifests.js": blossomManifestsJS, "blossom-upload.js": blossomUploadJS, "file-messages.js": fileMessagesJS, "private-services.js": privateServicesJS, "file-workspace.js": fileWorkspaceJS, "file-catalog.js": fileCatalogJS}

var scriptsVersion = func() string {
	names := make([]string, 0, len(scripts))
	for name := range scripts {
		names = append(names, name)
	}
	sort.Strings(names)
	digest := sha256.New()
	for _, name := range names {
		digest.Write([]byte(name))
		digest.Write([]byte{0})
		digest.Write([]byte(scripts[name]))
	}
	return hex.EncodeToString(digest.Sum(nil))[:12]
}()

func parseTemplates(a *App) (*template.Template, error) {
	funcs := template.FuncMap{
		"stylesheet": func() template.CSS { return template.CSS(styleCSS) },
		"scriptURL": func(name string) (string, error) {
			if _, ok := scripts[name]; !ok {
				return "", fmt.Errorf("unknown script %q", name)
			}
			return "/scripts/" + name + "?v=" + scriptsVersion, nil
		},
		"scriptsVersion": func() string { return scriptsVersion },
		"json": func(value any) string {
			encoded, err := json.Marshal(value)
			if err != nil {
				return "null"
			}
			return string(encoded)
		},
		"join":                strings.Join,
		"urlquery":            url.QueryEscape,
		"asMap":               valueMap,
		"str":                 plainString,
		"datetime":            datetime,
		"when":                when,
		"fileSize":            fileSize,
		"socialThreadPage":    socialThreadPage,
		"socialReactionCount": socialReactionCount,
		"socialReacted":       socialReacted,
		"socialContext":       socialContext,
		"socialQuery":         socialQuery,
		"socialURL":           socialURL,
		"socialThreadURL":     socialURL,
		"socialBody":          a.socialBody,
		"socialPreview":       socialPreview,
		"socialMediaURL":      socialMediaURL,
		"markdown":            a.markdown,
		"chatMarkdown":        a.chatMarkdown,
		"hasPrefix":           strings.HasPrefix,
		"npub":                identityNpub,
		"wsURL":               wsURL,
		"short":               short,
		"prompt":              promptPath,
		"pageAddress":         pageAddress,
		"signinURL":           signinURL,
		"railKind":            railKind,
		"repoView":            repoView,
		"relayItems":          func() []navItem { return relayNav },
		"manageItems":         func() []navItem { return manageNav },
		"navGroups":           navGroups,
		"add":                 func(a, b int) int { return a + b },
		"strs":                func(values ...string) []string { return values },
		"repoCommitURL":       repoCommitURL,
		"hasNextOffset":       hasNextOffset,
		"repoURL":             repoURL,
		"repoBreadcrumbs":     repoBreadcrumbs,
		"browsePageURL":       browsePageURL,
		"shortID":             shortID,
		"repoDate":            repoDate,
		"sourceHTML":          sourceHTML,
		"diffHTML":            diffHTML,
		"collabItems":         collaborationItems,
		"collabReplies":       collaborationReplies,
		"collabLabels":        collaborationLabels,
		"collabProposal":      collaborationProposal,
		"reviewDiff":          reviewDiffView,
		"reviewAnchor":        reviewAnchorQuery,
		"reviewLabel":         reviewAnchorLabel,
		"agentState":          agentState,
		"agentSince":          agentSince,
		"agentScope":          agentScope,
		"agentLabel":          agentLabel,
		"agentCounts":         agentCounts,
		"pendingRequests":     pendingRequests,
		"decidedRequests":     decidedRequests,
		"callbacksFor":        callbacksFor,
		"callbackState":       callbackState,
		"callbackKinds":       callbackKinds,
		"callbackCounts":      callbackCounts,
		"scopeList":           scopeList,
		"dateAfter":           dateAfter,
		"agentByKey":          agentByKey,
		"repoLines":           repoLines,
		"siteLines":           siteLines,
		"dateOf":              dateOf,
		"approvalViews":       approvalViews,
		"approvalCounts":      approvalCounts,
		"approvalDevices":     approvalDevices,
		"wikiHTML":            a.wikiHTML,
		"viewState":           customViewState,
		"viewKinds":           customViewKinds,
		"viewLanguages":       customViewLanguages,
		"wikiView":            wikiPageView,
		"wikiProposal":        wikiProposal,
		"wikiURL":             wikiURL,
		"roomItems":           roomItems,
		"roomRoot":            roomRoot,
		"roomMembers":         roomMembers,
		"roomContent":         roomContent,
		"age":                 age,
		"clock":               clock,
		"roomAdmin":           roomAdmin,

		"roomAttachmentContent": roomAttachmentContent,
		"roomAttachmentMarkup":  roomAttachmentMarkup,
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

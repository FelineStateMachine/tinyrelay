package webui

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/views"
	"github.com/skip2/go-qrcode"
)

// signerJS is the vendored MIT licensed Nostr signer bundle used for NIP-46
// and Nostr Connect browser flows. It contains no relay branding or secrets.
//
//go:embed signer.js
var signerJS []byte

// fixiJS is the vendored BSD-0 all-in-one bundle. It provides optional
// hypermedia fragments, focus-preserving swaps, small form state helpers, SSE
// handling, and fetch wrappers for pages that opt in with fx-* attributes.
//
//go:embed fixi.js
var fixiJS []byte

// Backend supplies relay identity and the method calls used by page handlers.
// Query receives the request context, method name, JSON arguments and actor
// public key, and returns the method's JSON-shaped result. Policy, URL, slug
// and identity describe the relay rendered into each page.
type Backend interface {
	Query(ctx context.Context, method string, params []json.RawMessage, actor string) (any, error)
	Policy() policy.Policy
	URL() string
	Slug() string
	Identity() string
}

// AccountPreferencesReader provides private, account-scoped preferences to
// server-rendered account pages. It is optional for small embedders.
type AccountPreferencesReader interface {
	SharePresence(context.Context, string) bool
}

// CustomViewSource is the optional backend summary of the custom views the
// renderers show in place of fenced code blocks: names and languages only.
type CustomViewSource interface {
	CustomViews() []views.View
}

// ActorResolver returns the authenticated public key associated with request.
// An empty key represents an anonymous browser request.
type ActorResolver func(request *http.Request) (string, error)

// QRProvider encodes text as an image for pages that show a connection QR.
type QRProvider func(text string) ([]byte, error)

type App struct {
	backend  Backend
	actor    ActorResolver
	qr       QRProvider
	tmpl     *template.Template
	version  string
	revision string
}

type Options struct {
	Actor ActorResolver
	QR    QRProvider
	// Version and Revision identify the relay build on the health page.
	Version, Revision string
}

type PageData struct {
	Activity      chatActivityPage
	SharePresence bool
	Version       string
	Revision      string
	Title         string
	URL           string
	Slug          string
	Identity      string
	Policy        policy.Policy
	Actor         string
	Owner         bool
	Tab           string
	Notice        string
	Error         string
	Methods       []string
	Event         any
	Invite        string
	View          string
	Feed          []any
	Base          string
	Query         url.Values
	Private       bool
	Path          string
	Readme        template.HTML
	Tree          []any
	// Merge is the browsewikimerge detail shown by a wiki page compare view.
	Merge any
	// Connections lists the ways to open this relay in client apps that the
	// viewer may see, with link placeholders already resolved.
	Connections []any
	// Rooms lists the rooms the viewer may see; the rooms rail renders it.
	Rooms []any
	// Callbacks lists the event callbacks shown beside each agent.
	Callbacks []any
	// CustomViews lists the owner's custom views on the Views page.
	CustomViews []any
	// Requests lists the access requests on the People page, pending first;
	// Review is true when the viewer may decide them.
	Requests []any
	Review   bool
}

func New(backend Backend, options Options) (*App, error) {
	if backend == nil {
		return nil, fmt.Errorf("webui backend is required")
	}
	qr := options.QR
	if qr == nil {
		qr = defaultQR
	}
	a := &App{backend: backend, actor: options.Actor, qr: qr, version: options.Version, revision: options.Revision}
	tmpl, err := parseTemplates(a)
	if err != nil {
		return nil, err
	}
	a.tmpl = tmpl
	return a, nil
}

// blocks is the block renderer for the custom views the backend reports,
// or nil when there are none so fenced blocks stay code.
func (a *App) blocks() blockRenderer {
	source, ok := a.backend.(CustomViewSource)
	if !ok {
		return nil
	}
	renderer := views.NewRenderer(source.CustomViews())
	if len(renderer.Languages) == 0 {
		return nil
	}
	return renderer.Block
}

func (a *App) markdown(source string) template.HTML {
	return renderMarkdownWith(source, a.blocks())
}

func (a *App) chatMarkdown(source string) template.HTML {
	return renderChatMarkdownWith(source, a.blocks())
}

func defaultQR(text string) ([]byte, error) {
	code, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		return nil, fmt.Errorf("encode QR: %w", err)
	}
	bitmap := code.Bitmap()
	var out strings.Builder
	size := len(bitmap)
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="100%%" height="100%%" fill="white"/>`, size, size)
	for y, row := range bitmap {
		for x, dark := range row {
			if dark {
				fmt.Fprintf(&out, `<rect x="%d" y="%d" width="1" height="1"/>`, x, y)
			}
		}
	}
	out.WriteString(`</svg>`)
	return []byte(out.String()), nil
}

func (a *App) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if a.denyPrivateEndpoint(writer, request) {
		return
	}
	if a.handleChatRoute(writer, request) || a.handleControlRoute(writer, request) || a.handleWebMCPRoute(writer, request) || a.handleBrowseRoute(writer, request) {
		return
	}
	if a.handleSocialRoute(writer, request) {
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if request.URL.Path == "/card.json" {
		a.card(writer)
		return
	}
	if request.URL.Path == "/people" {
		a.peopleJSON(writer, request)
		return
	}
	if request.URL.Path == "/connect.json" {
		a.connectJSON(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/e/") || strings.HasPrefix(request.URL.Path, "/a/") {
		if !a.pageFeatureEnabled(request.URL.Path) {
			http.NotFound(writer, request)
			return
		}
		a.eventPage(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/view/") {
		a.viewPage(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/invite/") {
		a.invitePage(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/scripts/") {
		source, ok := scripts[strings.TrimPrefix(request.URL.Path, "/scripts/")]
		if !ok {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("content-type", "application/javascript; charset=utf-8")
		if request.URL.Query().Get("v") == scriptsVersion {
			writer.Header().Set("cache-control", "public, max-age=31536000, immutable")
		} else {
			writer.Header().Set("cache-control", "no-cache")
		}
		_, _ = writer.Write([]byte(source))
		return
	}
	if request.URL.Path == "/signer.js" {
		writer.Header().Set("content-type", "application/javascript; charset=utf-8")
		_, _ = writer.Write([]byte(signerJS))
		return
	}
	if request.URL.Path == "/fixi.js" {
		writer.Header().Set("content-type", "application/javascript; charset=utf-8")
		_, _ = writer.Write(fixiJS)
		return
	}
	if request.URL.Path == "/sw.js" {
		writer.Header().Set("content-type", "application/javascript; charset=utf-8")
		writer.Header().Set("service-worker-allowed", "/")
		_, _ = writer.Write(serviceWorkerJS)
		return
	}
	if png, ok := map[string][]byte{"/icon-192.png": icon192PNG, "/icon-512.png": icon512PNG, "/icon-maskable-512.png": iconMaskablePNG, "/apple-touch-icon.png": appleTouchIconPNG, "/badge-96.png": badgePNG, "/screenshot-narrow.png": screenshotNarrowPNG, "/screenshot-wide.png": screenshotWidePNG}[request.URL.Path]; ok {
		writer.Header().Set("content-type", "image/png")
		writer.Header().Set("cache-control", "public, max-age=86400")
		_, _ = writer.Write(png)
		return
	}
	if request.URL.Path == "/icon.svg" || request.URL.Path == "/icon-mono.svg" {
		writer.Header().Set("content-type", "image/svg+xml; charset=utf-8")
		if request.URL.Path == "/icon.svg" {
			_, _ = writer.Write(iconSVG)
		} else {
			_, _ = writer.Write(iconMonoSVG)
		}
		return
	}
	if request.URL.Path == "/open" {
		a.openLink(writer, request)
		return
	}
	if request.URL.Path == "/manifest.webmanifest" {
		a.manifest(writer, request)
		return
	}
	if request.URL.Path == "/qr.svg" {
		a.qrImage(writer, request.URL.Query().Get("text"))
		return
	}
	if request.URL.Path == "/card.svg" {
		writer.Header().Set("content-type", "image/svg+xml; charset=utf-8")
		_, _ = fmt.Fprintf(writer, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 420 180"><rect width="420" height="180" fill="white"/><text x="20" y="70" font-family="sans-serif" font-size="28">%s</text><text x="20" y="112" font-family="monospace" font-size="14">%s</text></svg>`, template.HTMLEscapeString(a.backend.Policy().Name), template.HTMLEscapeString(a.backend.URL()))
		return
	}
	if request.URL.Path == "/card.nostr" {
		writer.Header().Set("content-type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(writer, "nostr:%s", identityNpub(a.backend.Identity()))
		return
	}
	if request.URL.Path == "/articles.json" {
		if !a.pageFeatureEnabled(request.URL.Path) {
			http.NotFound(writer, request)
			return
		}
		a.articlesJSON(writer, request)
		return
	}
	if request.URL.Path == "/feed" || request.URL.Path == "/feed.xml" {
		if !a.pageFeatureEnabled(request.URL.Path) {
			http.NotFound(writer, request)
			return
		}
		a.feed(writer, request)
		return
	}
	a.page(writer, request)
}

func (a *App) jobStatus(writer http.ResponseWriter, request *http.Request) {
	actor, err := a.resolveActor(request)
	if err != nil || actor == "" {
		http.Error(writer, "authentication required", http.StatusUnauthorized)
		return
	}
	writer.Header().Set("content-type", "text/event-stream; charset=utf-8")
	writer.Header().Set("cache-control", "no-cache")
	writer.Header().Set("x-content-type-options", "nosniff")
	flusher, ok := writer.(http.Flusher)
	if !ok {
		http.Error(writer, "streaming is unavailable", http.StatusNotImplemented)
		return
	}
	var previous string
	writeUpdate := func() bool {
		currentActor, actorErr := a.resolveActor(request)
		if actorErr != nil || currentActor == "" || currentActor != actor {
			return false
		}
		result, queryErr := a.backend.Query(request.Context(), "listjobs", nil, currentActor)
		fragment := jobsFragment(result, queryErr)
		if fragment == previous {
			return true
		}
		previous = fragment
		_, writeErr := fmt.Fprintf(writer, "data: %s\n\n", fragment)
		flusher.Flush()
		return writeErr == nil
	}
	if !writeUpdate() {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-request.Context().Done():
			return
		case <-ticker.C:
			if !writeUpdate() {
				return
			}
		}
	}
}

func (a *App) joinPolicy(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusOK, map[string]string{"terms": a.backend.Policy().JoinTerms})
}

func (a *App) claimInvite(writer http.ResponseWriter, request *http.Request) {
	actor, err := a.resolveActor(request)
	if err != nil || actor == "" {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	var code, termsHash string
	if strings.HasPrefix(request.Header.Get("content-type"), "application/json") {
		var body struct {
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid JSON request"})
			return
		}
		if len(body.Params) > 0 {
			_ = json.Unmarshal(body.Params[0], &code)
		}
		if len(body.Params) > 1 {
			_ = json.Unmarshal(body.Params[1], &termsHash)
		}
	} else if err := request.ParseForm(); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid form"})
		return
	} else {
		code = request.FormValue("code")
	}
	if code == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invite code is required"})
		return
	}
	params := []json.RawMessage{json.RawMessage(fmt.Sprintf("%q", code))}
	if termsHash != "" {
		params = append(params, json.RawMessage(fmt.Sprintf("%q", termsHash)))
	}
	result, err := a.backend.Query(request.Context(), "claiminvite", params, actor)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"result": result})
}

func (a *App) peopleJSON(writer http.ResponseWriter, request *http.Request) {
	actor, err := a.resolveActor(request)
	if err != nil {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	result, err := a.backend.Query(request.Context(), "listpeople", nil, actor)
	if err != nil {
		writeJSON(writer, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (a *App) connectJSON(writer http.ResponseWriter, request *http.Request) {
	actor, actorErr := a.resolveActor(request)
	if actorErr != nil || actor == "" {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	result, err := a.backend.Query(request.Context(), "listconnections", nil, actor)
	if err != nil {
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if request.URL.Query().Get("catalog") == "" {
		writeJSON(writer, http.StatusOK, result)
		return
	}
	// The Connect page reads the list and the card catalog with its session
	// so showing the page never asks the signer for a signature.
	catalog, err := a.backend.Query(request.Context(), "listconnectiontemplates", nil, actor)
	if err != nil {
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"connections": result, "catalog": catalog})
}

func (a *App) eventPage(writer http.ResponseWriter, request *http.Request) {
	actor, actorErr := a.resolveActor(request)
	if actorErr != nil {
		http.Error(writer, "authentication required", http.StatusUnauthorized)
		return
	}
	identifier := strings.TrimPrefix(request.URL.Path, "/e/")
	address := strings.HasPrefix(request.URL.Path, "/a/")
	if address {
		identifier = strings.TrimPrefix(request.URL.Path, "/a/")
	}
	if a.socialReferencePage(writer, request, identifier, address, actor) {
		return
	}
	method := "eventdetail"
	params := []json.RawMessage{json.RawMessage(fmt.Sprintf(`%q`, identifier))}
	if address {
		method = "queryevents"
		params = []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"#d":[%q],"limit":1}`, identifier))}
	}
	result, err := a.backend.Query(request.Context(), method, params, actor)
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	data := PageData{Event: result, View: identifier, Tab: "event"}
	data.Title = "Event " + shortID(identifier) + " | " + a.backend.Slug()
	if address {
		data.Feed = browseRows(result)
	}
	a.render(writer, request, data)
}

func (a *App) viewPage(writer http.ResponseWriter, request *http.Request) {
	name := strings.TrimPrefix(request.URL.Path, "/view/")
	writer.Header().Set("Cache-Control", "private, no-store")
	actor, err := a.resolveActor(request)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusUnauthorized)
		return
	}
	result, err := a.backend.Query(request.Context(), "listview", []json.RawMessage{json.RawMessage(fmt.Sprintf(`%q`, name))}, actor)
	if err != nil {
		status := http.StatusForbidden
		if strings.HasPrefix(err.Error(), "auth-required:") {
			status = http.StatusUnauthorized
		}
		if strings.HasPrefix(err.Error(), "not found:") {
			status = http.StatusNotFound
		}
		writeJSON(writer, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (a *App) browse(writer http.ResponseWriter, request *http.Request) {
	actor, actorErr := a.resolveActor(request)
	if actorErr != nil {
		http.Error(writer, "authentication required", http.StatusUnauthorized)
		return
	}
	path, method := request.URL.Path, ""
	switch path {
	case "/repos":
		method = "browserepos"
	case "/repo":
		method = "browserepo"
	case "/files":
		method = "browsefiles"
	case "/file":
		method = "browsefile"
	case "/approvals":
		method = "browseapprovals"
	case "/profile":
		method = "browseprofile"
	case "/wiki":
		method = "browsewiki"
	case "/manage/health":
		method = "browsestatus"
	}
	room := roomRoute(path)
	switch room.tab {
	case "rooms":
		method = "browserooms"
	case "room":
		method = "browseroom"
	case "thread":
		method = "browsethread"
	}
	if strings.HasPrefix(path, "/wiki/") {
		method = "browsewikipage"
	}
	params := []json.RawMessage{}
	pageQuery := request.URL.Query()
	query := map[string]any{"cursor": request.URL.Query().Get("cursor"), "limit": 50, "q": request.URL.Query().Get("q")}
	if value := request.URL.Query().Get("limit"); value != "" {
		if parsed, parseErr := strconv.Atoi(value); parseErr == nil && parsed > 0 {
			query["limit"] = parsed
		}
	}
	switch method {
	case "browsefiles":
		view := request.URL.Query().Get("view")
		if view == "" {
			view = "library"
		}
		query["view"] = view
		query["path"] = request.URL.Query().Get("path")
	case "browserepo":
		view := request.URL.Query().Get("view")
		if view == "" || view == "home" {
			view = "tree"
		}
		offset := 0
		if parsed, parseErr := strconv.Atoi(request.URL.Query().Get("offset")); parseErr == nil && parsed >= 0 {
			offset = parsed
		}
		backendView := view
		if view == "issues" {
			method = "browseissues"
			backendView = ""
		}
		if view == "prs" {
			method = "browsepulls"
			backendView = ""
		}
		if view == "issue" {
			method = "browseissue"
			backendView = ""
		}
		if view == "pr" {
			method = "browsepull"
			backendView = ""
		}
		eventID := request.URL.Query().Get("id")
		if eventID == "" && (view == "issue" || view == "pr") {
			eventID = request.URL.Query().Get("path")
		}
		query = map[string]any{"owner": request.URL.Query().Get("owner"), "repo": request.URL.Query().Get("repo"), "event": eventID, "q": request.URL.Query().Get("q"), "state": request.URL.Query().Get("status"), "label": request.URL.Query().Get("label"), "cursor": request.URL.Query().Get("cursor"), "ref": request.URL.Query().Get("ref"), "path": request.URL.Query().Get("path"), "view": backendView, "offset": offset, "limit": 100}
	case "browsefile":
		query = map[string]any{"hash": request.URL.Query().Get("hash")}
		if query["hash"] == "" {
			query["hash"] = request.URL.Query().Get("sha")
		}
	case "browseprofile":
		query = map[string]any{"pubkey": actor}
	case "browseapprovals":
		state := request.URL.Query().Get("state")
		if state == "" {
			state = "all"
		}
		query = map[string]any{"cursor": query["cursor"], "limit": query["limit"], "state": state}
	case "browsewiki":
		query["author"] = request.URL.Query().Get("author")
	case "browsewikipage":
		// The name travels in the query as well so the page template can
		// offer to create a page that does not exist yet.
		pageQuery.Set("d", wikiPageName(path))
		query = map[string]any{"d": pageQuery.Get("d"), "author": request.URL.Query().Get("author"), "version": request.URL.Query().Get("version")}
	case "browsestatus":
		query = map[string]any{}
	case "browserooms":
		query = map[string]any{"cursor": request.URL.Query().Get("cursor"), "limit": 100}
	case "browseroom", "browsethread":
		query = map[string]any{"id": room.id, "event": room.event, "cursor": request.URL.Query().Get("cursor"), "limit": 50}
	}
	raw, _ := json.Marshal(query)
	params = append(params, raw)
	var tree []any
	if method == "browserepo" && request.URL.Query().Get("view") == "file" {
		tree = a.siblings(request.Context(), actor, request.URL.Query())
	}
	result, err, typedRoom := a.typedRoomResult(request.Context(), room, pageQuery, actor)
	if !typedRoom {
		result, err = a.backend.Query(request.Context(), method, params, actor)
	}
	if err != nil {
		data := PageData{Tab: browseTab(path), Error: err.Error(), Query: pageQuery, View: room.id}
		if room.tab != "" {
			data.Rooms = a.roomList(request.Context(), actor)
		}
		a.render(writer, request, data)
		return
	}
	if method == "browserepo" && pageQuery.Get("view") == "" {
		pageQuery.Set("view", "tree")
	}
	if method == "browseapprovals" && pageQuery.Get("id") != "" {
		// A notification opens one request; show it even when it has left
		// the first page.
		result = a.includeApproval(request.Context(), actor, result, pageQuery.Get("id"))
	}
	data := PageData{Tab: browseTab(path), Feed: browseRows(result), Event: result, Query: pageQuery, View: room.id}
	data.Title = browseTitle(path, pageQuery, result, a.backend.Slug())
	if id := pageQuery.Get("merge"); method == "browsewikipage" && len(id) == 64 {
		raw, _ := json.Marshal(map[string]any{"id": id})
		if merge, mergeErr := a.backend.Query(request.Context(), "browsewikimerge", []json.RawMessage{raw}, actor); mergeErr == nil {
			data.Merge = merge
		} else {
			data.Notice = mergeErr.Error()
		}
	}
	switch room.tab {
	case "rooms":
		data.Rooms = data.Feed
	case "room", "thread":
		data.Rooms = a.roomList(request.Context(), actor)
		data.Activity = a.chatActivity(request.Context(), actor, room.id, plainString(valueMap(valueMap(result)["root"])["id"]))
	}
	if method == "browserepo" && pageQuery.Get("view") == "home" {
		data.Readme = a.readme(request.Context(), actor, pageQuery)
	}
	data.Tree = tree
	a.render(writer, request, data)
}

func browseTitle(path string, query url.Values, result any, slug string) string {
	label := map[string]string{"/repos": "Repositories", "/files": "Files", "/file": "File", "/approvals": "Approvals", "/profile": "Profile", "/wiki": "Wiki", "/rooms": "Rooms"}[path]
	if strings.HasPrefix(path, "/wiki/") {
		label = plainString(valueMap(result)["title"])
		if label == "" {
			label = query.Get("d")
		}
		label += " | wiki"
	}
	if room := roomRoute(path); room.tab == "room" || room.tab == "thread" {
		label = "#" + room.id
		if room.tab == "thread" {
			label += " | thread"
		}
	}
	if path == "/repo" {
		label = query.Get("repo")
		if label == "" {
			label = plainString(valueMap(result)["identifier"])
		}
		if label == "" {
			label = "Repository"
		}
		label += " | " + repoView(query)
	}
	if label == "" {
		label = "Browse"
	}
	return label + " | " + slug
}

// readme renders README.md from the requested ref for the repository entry
// page. A missing or binary README simply yields no content.
func (a *App) readme(ctx context.Context, actor string, query url.Values) template.HTML {
	for _, name := range []string{"README.md", "readme.md", "README"} {
		raw, _ := json.Marshal(map[string]any{"owner": query.Get("owner"), "repo": query.Get("repo"), "ref": query.Get("ref"), "path": name, "view": "file", "limit": 1})
		result, err := a.backend.Query(ctx, "browserepo", []json.RawMessage{raw}, actor)
		if err != nil {
			continue
		}
		page := valueMap(result)
		content, _ := page["content"].(string)
		if binary, _ := page["binary"].(bool); binary || content == "" {
			continue
		}
		return renderRepositoryMarkdownWith(content, query, a.blocks())
	}
	return ""
}

// siblings lists the directory that holds the file being previewed so the
// tree pane stays populated beside the file.
func (a *App) siblings(ctx context.Context, actor string, query url.Values) []any {
	dir := ""
	if slash := strings.LastIndex(query.Get("path"), "/"); slash >= 0 {
		dir = query.Get("path")[:slash]
	}
	raw, _ := json.Marshal(map[string]any{"owner": query.Get("owner"), "repo": query.Get("repo"), "ref": query.Get("ref"), "path": dir, "view": "tree", "limit": 100})
	result, err := a.backend.Query(ctx, "browserepo", []json.RawMessage{raw}, actor)
	if err != nil {
		return nil
	}
	entries, _ := valueMap(result)["entries"].([]any)
	return entries
}

func browseTab(path string) string {
	if strings.HasPrefix(path, "/wiki/") {
		return "wikipage"
	}
	if room := roomRoute(path); room.tab != "" {
		return room.tab
	}
	switch path {
	case "/wiki":
		return "wiki"
	case "/repos":
		return "repos"
	case "/repo":
		return "repo"
	case "/files":
		return "files"
	case "/file":
		return "file"
	case "/approvals":
		return "approvals"
	case "/profile":
		return "profile"
	default:
		return "health"
	}
}
func browseRows(value any) []any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var rows []any
	if json.Unmarshal(encoded, &rows) == nil {
		return rows
	}
	var object map[string]any
	if json.Unmarshal(encoded, &object) == nil {
		if items, ok := object["items"].([]any); ok {
			return items
		}
	}
	return []any{value}
}

func (a *App) invitePage(writer http.ResponseWriter, request *http.Request) {
	code := strings.TrimPrefix(request.URL.Path, "/invite/")
	a.render(writer, request, PageData{Invite: code, Tab: "invite"})
}

// render fills the shared page data, applies private relay masking, and
// writes the page. Every HTML response goes through here.
func (a *App) render(writer http.ResponseWriter, request *http.Request, data PageData) {
	actor, _ := a.resolveActor(request)
	if data.Title == "" {
		data.Title = a.backend.Slug()
	}
	data.URL, data.Slug, data.Identity, data.Policy, data.Actor, data.Base = a.backend.URL(), a.backend.Slug(), a.backend.Identity(), a.backend.Policy(), actor, requestPrefix(request)
	data.Owner = actor != "" && actor == data.Policy.Owner
	if actor != "" && data.Tab == "account" {
		if preferences, ok := a.backend.(AccountPreferencesReader); ok {
			data.SharePresence = preferences.SharePresence(request.Context(), actor)
		}
	}
	data.Version, data.Revision = a.version, a.revision
	data.Methods = supportedMethods
	if data.Query == nil {
		data.Query = request.URL.Query()
	}
	data.Path = strings.TrimPrefix(request.URL.Path, data.Base)
	a.privatePageData(&data, request, actor)
	if !data.Private && data.Actor == "" && (railKind(data.Tab) == "manage" || data.Tab == "profile" || data.Tab == "account") {
		// Management pages and the profile editor are for signed-in people
		// only. Guests see the sign-in page at the same address and come back
		// after signing in.
		data.Tab, data.Notice = "signin", "Sign in to manage this relay."
		if data.Path == "/profile" {
			data.Notice = "Sign in to edit your profile."
		} else if data.Path == "/account" {
			data.Notice = "Sign in to open your account."
		}
		data.Event, data.Feed, data.Error = nil, nil, ""
	}
	writer.Header().Set("Cache-Control", "private, no-store")
	writer.Header().Set("content-type", "text/html; charset=utf-8")
	var rendered bytes.Buffer
	if err := a.tmpl.ExecuteTemplate(&rendered, "page", data); err != nil {
		http.Error(writer, "render page: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = writer.Write([]byte(injectBase(rendered.String(), data.Base)))
}

// manifest describes the installable app for this relay and tenant prefix.
func (a *App) manifest(writer http.ResponseWriter, request *http.Request) {
	name := a.backend.Policy().Name
	if name == "" || privatePolicyEnabled(a.backend.Policy()) {
		name = a.backend.Slug()
	}
	encoded, _ := json.Marshal(name)
	body := strings.ReplaceAll(manifestTemplate, `"NAME"`, string(encoded))
	body = strings.ReplaceAll(body, "BASE/", requestPrefix(request)+"/")
	// A manifest carries one set of launch colours. The page links the
	// variant that matches the active theme so the installed app's splash
	// screen and status bar do not flash light on a dark phone.
	theme, background := "#f4f2ec", "#fbfaf7"
	if request.URL.Query().Get("theme") == "dark" {
		theme, background = "#1e1d1a", "#171614"
	}
	body = strings.ReplaceAll(body, "THEME", theme)
	body = strings.ReplaceAll(body, "BACKGROUND", background)
	writer.Header().Set("content-type", "application/manifest+json; charset=utf-8")
	_, _ = writer.Write([]byte(body))
}

func (a *App) qrImage(writer http.ResponseWriter, text string) {
	if a.qr == nil || text == "" {
		http.Error(writer, "QR generation is not configured", http.StatusNotImplemented)
		return
	}
	image, err := a.qr(text)
	if err != nil {
		http.Error(writer, "QR generation failed", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("content-type", "image/svg+xml; charset=utf-8")
	_, _ = writer.Write(image)
}

func (a *App) page(writer http.ResponseWriter, request *http.Request) {
	tab := tabForPath(request.URL.Path)
	actor, err := a.resolveActor(request)
	if err != nil {
		actor = ""
	}
	data := PageData{Tab: tab, Query: request.URL.Query()}
	if tab == "inbox" || tab == "outbox" || tab == "search" || tab == "articles" || tab == "sites" {
		feed, feedErr := a.publicFeed(request.Context(), tab, actor, request.URL.Query())
		if feedErr != nil {
			data.Error = feedErr.Error()
		} else if tab == "sites" {
			data.Feed = a.siteRows(feed)
		} else {
			data.Feed = feed
		}
	}
	if tab == "home" {
		if result, err := a.backend.Query(request.Context(), "connections", nil, actor); err == nil {
			data.Connections = expandConnections(browseRows(result), PageData{URL: a.backend.URL(), Identity: a.backend.Identity(), Policy: a.backend.Policy(), Actor: actor})
		}
	}
	if tab == "agents" && actor != "" {
		a.agentsPage(request, actor, &data)
	}
	if tab == "people" && actor != "" {
		a.peoplePage(request, actor, &data)
	}
	if tab == "views" && actor != "" {
		// The listing is owner-only; anyone else keeps the built-in controls.
		if result, err := a.backend.Query(request.Context(), "listcustomviews", nil, actor); err == nil {
			data.CustomViews = browseRows(result)
		}
	}
	if tab != "" {
		data.Title = tab + " | " + a.backend.Slug()
	}
	a.render(writer, request, data)
}

// agentsPage loads the agent list for the table and cards, then the grant
// and recent events of one agent: the one named by ?agent=, else the first.
// Both queries are gated for the owner and moderators by the backend.
func (a *App) agentsPage(request *http.Request, actor string, data *PageData) {
	result, err := a.backend.Query(request.Context(), "listagents", nil, actor)
	if err != nil {
		data.Error = err.Error()
		return
	}
	data.Feed = browseRows(result)
	// Callbacks are shown per agent; a failure here leaves the row empty
	// rather than hiding the agents.
	if callbacks, err := a.backend.Query(request.Context(), "listcallbacks", nil, actor); err == nil {
		data.Callbacks = browseRows(callbacks)
	}
	selected := request.URL.Query().Get("agent")
	if selected == "" {
		// Without a choice, show the first agent that can still publish.
		for _, agent := range data.Feed {
			if selected == "" || agentState(agent) == "active" {
				selected = plainString(valueMap(agent)["pubkey"])
			}
			if agentState(agent) == "active" {
				break
			}
		}
	}
	if selected == "" {
		return
	}
	raw, _ := json.Marshal(map[string]any{"agent": selected})
	detail, err := a.backend.Query(request.Context(), "browseagent", []json.RawMessage{raw}, actor)
	if err != nil {
		data.Error = err.Error()
		return
	}
	data.Event = detail
}

// peoplePage loads the access requests for the owner and moderators. A
// member's request is refused by the backend, and the section stays out.
func (a *App) peoplePage(request *http.Request, actor string, data *PageData) {
	result, err := a.backend.Query(request.Context(), "listjoinrequests", nil, actor)
	if err != nil {
		return
	}
	data.Review = true
	for _, row := range browseRows(result) {
		if plainString(valueMap(row)["pubkey"]) != "" {
			data.Requests = append(data.Requests, row)
		}
	}
}

// pendingRequests keeps the access requests that wait for a decision.
func pendingRequests(rows []any) []any {
	var pending []any
	for _, row := range rows {
		if plainString(valueMap(row)["status"]) == "pending" {
			pending = append(pending, row)
		}
	}
	return pending
}

// decidedRequests keeps the last decided access requests, newest first as
// the backend lists them.
func decidedRequests(rows []any) []any {
	var decided []any
	for _, row := range rows {
		if status := plainString(valueMap(row)["status"]); status != "" && status != "pending" {
			decided = append(decided, row)
		}
		if len(decided) == 10 {
			break
		}
	}
	return decided
}

// agentState folds a grant's marks into one word for data-state: revoked
// covers expired grants too, since neither lets the agent publish again
// without a new grant.
func agentState(agent any) string {
	values := valueMap(agent)
	switch {
	case unixSeconds(values["revoked"]) > 0:
		return "revoked"
	case unixSeconds(values["expires"]) > 0 && unixSeconds(values["expires"]) <= time.Now().Unix():
		return "revoked"
	case values["paused"] == true:
		return "paused"
	default:
		return "active"
	}
}

// agentSince is the short time shown beside the state: when the grant was
// revoked or expired, else the agent's newest event.
func agentSince(agent any) string {
	values := valueMap(agent)
	if revoked := unixSeconds(values["revoked"]); revoked > 0 {
		return short(revoked)
	}
	if expires := unixSeconds(values["expires"]); expires > 0 && expires <= time.Now().Unix() {
		return "expired " + short(expires)
	}
	if last := short(values["lastEvent"]); last != "" {
		return last
	}
	return ""
}

// agentLabel is the grant's name, or the shortened key when it has none.
func agentLabel(agent any) string {
	values := valueMap(agent)
	if name := plainString(values["name"]); name != "" {
		return name
	}
	return shortID(plainString(values["pubkey"]))
}

// scopeList joins one list from the grant scope: rooms or kinds.
func scopeList(agent any, key string) string {
	items, _ := valueMap(valueMap(agent)["scope"])[key].([]any)
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, plainString(item))
	}
	return strings.Join(parts, ", ")
}

// agentScope summarises a grant on one line for the agent table.
func agentScope(agent any) string {
	scope := valueMap(valueMap(agent)["scope"])
	var parts []string
	if rooms := scopeList(agent, "rooms"); rooms != "" {
		parts = append(parts, "rooms: "+rooms)
	}
	if repos, _ := scope["repos"].([]any); len(repos) > 0 {
		names := make([]string, 0, len(repos))
		for _, repo := range repos {
			values := valueMap(repo)
			names = append(names, plainString(values["identifier"])+" ("+plainString(values["level"])+")")
		}
		parts = append(parts, "repos: "+strings.Join(names, ", "))
	}
	if wiki := plainString(scope["wiki"]); wiki != "" {
		parts = append(parts, "wiki: "+wiki)
	}
	if kinds := scopeList(agent, "kinds"); kinds != "" {
		parts = append(parts, "kinds: "+kinds)
	}
	if sites, _ := scope["sites"].([]any); len(sites) > 0 {
		labels := make([]string, 0, len(sites))
		for _, site := range sites {
			labels = append(labels, plainString(valueMap(site)["label"]))
		}
		parts = append(parts, "sites: "+strings.Join(labels, ", "))
	}
	if len(parts) == 0 {
		return "profile and relay list only"
	}
	return strings.Join(parts, " | ")
}

// agentCounts tallies agents by state for the panel.
func agentCounts(agents []any) map[string]int {
	counts := map[string]int{"active": 0, "paused": 0, "revoked": 0}
	for _, agent := range agents {
		counts[agentState(agent)]++
	}
	return counts
}

// callbacksFor picks the callbacks one key registered.
func callbacksFor(callbacks []any, pubkey string) []any {
	var out []any
	for _, callback := range callbacks {
		if plainString(valueMap(callback)["owner"]) == pubkey {
			out = append(out, callback)
		}
	}
	return out
}

// callbackState is active or paused; a paused callback carries the reason
// in its lastStatus.
func callbackState(callback any) string {
	if valueMap(callback)["paused"] == true {
		return "paused"
	}
	return "active"
}

// callbackKinds lists the kinds a callback's filter names.
func callbackKinds(callback any) string {
	kinds, _ := valueMap(valueMap(callback)["filter"])["kinds"].([]any)
	parts := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		parts = append(parts, plainString(kind))
	}
	return strings.Join(parts, ", ")
}

// callbackCounts tallies callbacks by state for the panel.
func callbackCounts(callbacks []any) map[string]int {
	counts := map[string]int{"active": 0, "paused": 0}
	for _, callback := range callbacks {
		counts[callbackState(callback)]++
	}
	return counts
}

// dateAfter is the date field value for a day count from today, in UTC.
func dateAfter(days int) string {
	return time.Now().UTC().AddDate(0, 0, days).Format("2006-01-02")
}

// agentByKey finds one grant in the agents list, for the form that replaces
// it. An unknown or empty key yields nil, so the form stays blank.
func agentByKey(agents []any, pubkey string) any {
	if !hexID.MatchString(pubkey) {
		return nil
	}
	for _, agent := range agents {
		if plainString(valueMap(agent)["pubkey"]) == pubkey {
			return agent
		}
	}
	return nil
}

// repoLines renders a grant's repositories one per line, the way the form
// takes them: owner:identifier:level.
func repoLines(agent any) string {
	repos, _ := valueMap(valueMap(agent)["scope"])["repos"].([]any)
	lines := make([]string, 0, len(repos))
	for _, repo := range repos {
		r := valueMap(repo)
		lines = append(lines, plainString(r["owner"])+":"+plainString(r["identifier"])+":"+plainString(r["level"]))
	}
	return strings.Join(lines, "\n")
}

// siteLines renders a grant's sites one per line, the way the form takes
// them: the label, then ttl=<days> and encrypted when set.
func siteLines(agent any) string {
	sites, _ := valueMap(valueMap(agent)["scope"])["sites"].([]any)
	lines := make([]string, 0, len(sites))
	for _, site := range sites {
		s := valueMap(site)
		line := plainString(s["label"])
		if ttl := plainString(s["ttl"]); ttl != "" && ttl != "0" {
			line += " ttl=" + ttl
		}
		if s["encrypted"] == true {
			line += " encrypted"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// dateOf renders a unix time as the form's date value.
func dateOf(value any) string {
	seconds := unixSeconds(value)
	if seconds <= 0 {
		return ""
	}
	return time.Unix(seconds, 0).UTC().Format("2006-01-02")
}

var supportedMethods = []string{
	"supportedmethods", "stats", "getpolicy", "setpolicy", "listaudit", "listviews", "listmembers", "listpeople", "listjoinrequests", "approvejoin", "denyjoin", "setmember", "allowpubkey", "unrulepubkey", "removemember", "createinvite", "listinvites", "revokeinvite", "listclaims", "createclaim", "deleteclaim", "removesubtree", "listbannedpubkeys", "listallowedpubkeys", "banpubkey", "listreports", "resolvereport", "banevent", "allowevent", "listeventsneedingmoderation", "blockip", "unblockip", "listblockedips", "exportconfig", "importconfig", "planconfig", "listblobs", "deleteblob", "listbannedevents", "deleteevent", "listrecentevents", "searchevents", "pinevent", "unpinevent", "listpins", "allowkind", "disallowkind", "unrulekind", "storagestats", "gitstorage", "setretention", "listretention", "purgekind", "listallowedkinds", "listblockedkinds", "notifytest", "resetrules", "listpresets", "listconnectiontemplates", "listconnections", "setconnections", "applypreset", "forkrelay", "pullfrom", "pullstatus", "listjobs", "deliverystatus", "addjob", "removejob", "runjob", "backfill", "transferowner", "listdumps", "deletedump", "dumpnow", "backupnow", "listbackups", "deletebackup", "changerelayname", "changerelaydescription", "changerelayicon", "adddomain", "setdomainsite", "checkdomain", "removedomain", "listdomains", "listcallbacks", "addcallback", "removecallback", "pausecallback", "resumecallback",
}

func (a *App) card(writer http.ResponseWriter) {
	writer.Header().Set("content-type", "application/json; charset=utf-8")
	_ = json.NewEncoder(writer).Encode(map[string]any{"name": a.backend.Policy().Name, "description": a.backend.Policy().Description, "url": a.backend.URL(), "slug": a.backend.Slug(), "identity": a.backend.Identity()})
}

func (a *App) rpc(writer http.ResponseWriter, request *http.Request) {
	if a.actor == nil {
		http.Error(writer, "authentication is not configured", http.StatusUnauthorized)
		return
	}
	actor, err := a.actor(request)
	if err != nil || actor == "" {
		http.Error(writer, "authentication required", http.StatusUnauthorized)
		return
	}
	var method string
	var params []json.RawMessage
	if strings.HasPrefix(request.Header.Get("content-type"), "application/json") {
		var body struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "invalid JSON request", http.StatusBadRequest)
			return
		}
		method, params = body.Method, body.Params
	} else if err := request.ParseForm(); err != nil {
		http.Error(writer, "invalid form", http.StatusBadRequest)
		return
	} else {
		method = request.FormValue("method")
		params = make([]json.RawMessage, 0, len(request.Form["param"]))
		for _, value := range request.Form["param"] {
			params = append(params, json.RawMessage(value))
		}
	}
	if method == "" {
		http.Error(writer, "method is required", http.StatusBadRequest)
		return
	}
	result, err := a.backend.Query(request.Context(), method, params, actor)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"result": result})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("content-type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func (a *App) resolveActor(request *http.Request) (string, error) {
	if a.actor == nil {
		return "", nil
	}
	return a.actor(request)
}

func pathPrefix(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "r" && parts[1] != "" {
		return "/r/" + parts[1]
	}
	return ""
}

func requestPrefix(request *http.Request) string {
	if request != nil && request.RequestURI != "" {
		if parsed, err := url.ParseRequestURI(request.RequestURI); err == nil {
			if prefix := pathPrefix(parsed.Path); prefix != "" {
				return prefix
			}
		}
	}
	if request == nil || request.URL == nil {
		return ""
	}
	return pathPrefix(request.URL.Path)
}

func tabForPath(path string) string {
	path = strings.TrimPrefix(path, pathPrefix(path))
	if path == "/" || path == "" {
		return "home"
	}
	path = strings.Trim(path, "/")
	if strings.HasPrefix(path, "manage/") {
		path = strings.TrimPrefix(path, "manage/")
	}
	if strings.HasPrefix(path, "wiki/") {
		return "wikipage"
	}
	switch path {
	case "manage":
		return "owner"
	case "wiki":
		return "wiki"
	case "repos":
		return "repos"
	case "repo":
		return "repo"
	case "files":
		return "files"
	case "file":
		return "file"
	case "approvals":
		return "approvals"
	case "profile":
		return "profile"
	case "account":
		return "account"
	case "signin", "sites":
		return path
	case "people", "agents", "moderation", "rules", "identity", "connect", "data", "sync", "views", "health", "owner":
		return path
	case "inbox", "outbox", "private", "chat", "media", "search", "articles", "dm", "quiet", "site", "marmot", "grasp", "terms":
		return path
	default:
		return "home"
	}
}

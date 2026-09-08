// Package webui contains the deliberately plain HTML surface for a hosted
// relay. It owns presentation and form routing; relay policy and mutations
// remain in the daemon supplied Backend.
package webui

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
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

type Backend interface {
	Query(context.Context, string, []json.RawMessage, string) (any, error)
	Policy() policy.Policy
	URL() string
	Slug() string
	Identity() string
}

type ActorResolver func(*http.Request) (string, error)
type QRProvider func(string) ([]byte, error)

type App struct {
	backend Backend
	actor   ActorResolver
	qr      QRProvider
	tmpl    *template.Template
}

type Options struct {
	Actor ActorResolver
	QR    QRProvider
}

type PageData struct {
	Title    string
	URL      string
	Slug     string
	Identity string
	Policy   policy.Policy
	Actor    string
	Owner    bool
	Tab      string
	Notice   string
	Error    string
	Methods  []string
	Event    any
	Invite   string
	View     string
	Feed     []any
	Base     string
	Query    url.Values
	Private  bool
	Path     string
	Readme   template.HTML
	Tree     []any
}

func New(backend Backend, options Options) (*App, error) {
	if backend == nil {
		return nil, fmt.Errorf("webui backend is required")
	}
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	qr := options.QR
	if qr == nil {
		qr = defaultQR
	}
	return &App{backend: backend, actor: options.Actor, qr: qr, tmpl: tmpl}, nil
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
	if request.Method == http.MethodPost && request.URL.Path == "/manage/rpc" {
		a.rpc(writer, request)
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/api/join-policy" {
		a.joinPolicy(writer)
		return
	}
	if request.Method == http.MethodPost && request.URL.Path == "/api/invites/claim" {
		a.claimInvite(writer, request)
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/manage/jobs/status" {
		a.jobStatus(writer, request)
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/webmcp.js" {
		a.webMCP(writer, request)
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/webmcp/query" {
		a.webMCPQuery(writer, request)
		return
	}
	if request.Method == http.MethodGet && (request.URL.Path == "/repos" || request.URL.Path == "/repo" || request.URL.Path == "/files" || request.URL.Path == "/file" || request.URL.Path == "/manage/status") {
		a.browse(writer, request)
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
	if request.URL.Path == "/connect/fragment" {
		a.connectFragment(writer, request)
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
	if request.URL.Path == "/icon.svg" || request.URL.Path == "/icon-mono.svg" {
		writer.Header().Set("content-type", "image/svg+xml; charset=utf-8")
		if request.URL.Path == "/icon.svg" {
			_, _ = writer.Write(iconSVG)
		} else {
			_, _ = writer.Write(iconMonoSVG)
		}
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

func jobsFragment(result any, queryErr error) string {
	if queryErr != nil {
		return `<tbody><tr><td role="status">` + template.HTMLEscapeString(queryErr.Error()) + `</td></tr></tbody>`
	}
	rows := make([][]string, 0)
	for _, row := range browseRows(result) {
		values, _ := row.(map[string]any)
		rows = append(rows, []string{plainString(nestedValue(values, "spec", "id")), plainString(firstValue(values, "phase", "status")), plainString(firstValue(values, "finished", "started"))})
	}
	return tableRows([]string{"Job", "Status", "Updated"}, rows)
}

func nestedValue(values map[string]any, keys ...string) any {
	var current any = values
	for _, key := range keys {
		m, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = m[key]
	}
	return current
}

func firstValue(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok && value != nil {
			return value
		}
	}
	return ""
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
	writeJSON(writer, http.StatusOK, result)
}

func (a *App) connectFragment(writer http.ResponseWriter, request *http.Request) {
	actor, actorErr := a.resolveActor(request)
	if actorErr != nil || actor == "" {
		http.Error(writer, "authentication required", http.StatusUnauthorized)
		return
	}
	result, err := a.backend.Query(request.Context(), "listconnections", nil, actor)
	if err != nil {
		http.Error(writer, "connections unavailable", http.StatusNotFound)
		return
	}
	rows := make([][]string, 0)
	for _, row := range browseRows(result) {
		values, _ := row.(map[string]any)
		rows = append(rows, []string{plainString(firstValue(values, "label", "title", "name", "template")), plainString(firstValue(values, "url", "href", "relay")), plainString(firstValue(values, "status", "visibility"))})
	}
	fragment := `<table id="connections-preview" aria-live="polite">` + tableRows([]string{"Label", "Relay", "Status"}, rows) + `</table>`
	writer.Header().Set("content-type", "text/html; charset=utf-8")
	_, _ = writer.Write([]byte(fragment))
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
	case "/manage/status":
		method = "browsestatus"
	}
	params := []json.RawMessage{}
	query := map[string]any{"cursor": request.URL.Query().Get("cursor"), "limit": 50, "q": request.URL.Query().Get("q")}
	if value := request.URL.Query().Get("limit"); value != "" {
		if parsed, parseErr := strconv.Atoi(value); parseErr == nil && parsed > 0 {
			query["limit"] = parsed
		}
	}
	switch method {
	case "browserepo":
		view := request.URL.Query().Get("view")
		if view == "" || view == "home" {
			view = "tree"
		}
		offset := 0
		if parsed, parseErr := strconv.Atoi(request.URL.Query().Get("offset")); parseErr == nil && parsed >= 0 {
			offset = parsed
		}
		query = map[string]any{"owner": request.URL.Query().Get("owner"), "repo": request.URL.Query().Get("repo"), "ref": request.URL.Query().Get("ref"), "path": request.URL.Query().Get("path"), "view": view, "offset": offset, "limit": 50}
	case "browsefile":
		query = map[string]any{"hash": request.URL.Query().Get("hash")}
		if query["hash"] == "" {
			query["hash"] = request.URL.Query().Get("sha")
		}
	case "browsestatus":
		query = map[string]any{}
	}
	raw, _ := json.Marshal(query)
	params = append(params, raw)
	var tree []any
	if method == "browserepo" && request.URL.Query().Get("view") == "file" {
		tree = a.siblings(request.Context(), actor, request.URL.Query())
	}
	result, err := a.backend.Query(request.Context(), method, params, actor)
	if err != nil {
		a.render(writer, request, PageData{Tab: browseTab(path), Error: err.Error(), Query: request.URL.Query()})
		return
	}
	pageQuery := request.URL.Query()
	if method == "browserepo" && pageQuery.Get("view") == "" {
		pageQuery.Set("view", "tree")
	}
	data := PageData{Tab: browseTab(path), Feed: browseRows(result), Event: result, Query: pageQuery}
	if method == "browserepo" && pageQuery.Get("view") == "home" {
		data.Readme = a.readme(request.Context(), actor, pageQuery)
	}
	data.Tree = tree
	a.render(writer, request, data)
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
		return renderRepositoryMarkdown(content, query)
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
	switch path {
	case "/repos":
		return "repos"
	case "/repo":
		return "repo"
	case "/files":
		return "files"
	case "/file":
		return "file"
	case "/manage/status":
		return "status"
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
	data.Methods = supportedMethods
	if data.Query == nil {
		data.Query = request.URL.Query()
	}
	data.Path = strings.TrimPrefix(request.URL.Path, data.Base)
	a.privatePageData(&data, request, actor)
	if !data.Private && data.Actor == "" && railKind(data.Tab) == "manage" && data.Tab != "tools" {
		// Management pages are for signed-in people only. Guests see the
		// sign-in page at the same address and come back after signing in.
		data.Tab, data.Notice = "signin", "Sign in to manage this relay."
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

func identityNpub(identity string) string {
	decoded, err := hex.DecodeString(identity)
	if err != nil || len(decoded) != 32 {
		return identity
	}
	return encodeNpub(decoded)
}

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func encodeNpub(data []byte) string {
	fiveBits := convertBits(data)
	values := append(append([]byte{}, fiveBits...), 0, 0, 0, 0, 0, 0)
	polymod := bech32Polymod(append([]byte{3, 3, 3, 3, 0, 14, 16, 21, 2}, values...)) ^ 1
	checksum := make([]byte, 6)
	for i := range checksum {
		checksum[i] = byte(polymod >> uint(5*(5-i)) & 31)
	}
	var out strings.Builder
	out.WriteString("npub1")
	for _, value := range append(fiveBits, checksum...) {
		out.WriteByte(bech32Charset[value])
	}
	return out.String()
}

func convertBits(data []byte) []byte {
	result := make([]byte, 0, 52)
	accumulator, bits := 0, 0
	for _, value := range data {
		accumulator = (accumulator << 8) | int(value)
		bits += 8
		for bits >= 5 {
			bits -= 5
			result = append(result, byte(accumulator>>bits&31))
		}
	}
	if bits > 0 {
		result = append(result, byte(accumulator<<(5-bits)&31))
	}
	return result
}

func bech32Polymod(values []byte) uint64 {
	generators := [...]uint64{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	checksum := uint64(1)
	for _, value := range values {
		top := checksum >> 25
		checksum = (checksum&0x1ffffff)<<5 ^ uint64(value)
		for i, generator := range generators {
			if top>>uint(i)&1 != 0 {
				checksum ^= generator
			}
		}
	}
	return checksum
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
	if tab == "inbox" || tab == "outbox" || tab == "search" || tab == "articles" || tab == "home" || tab == "sites" {
		feed, feedErr := a.publicFeed(request.Context(), tab, actor, request.URL.Query())
		if feedErr != nil {
			data.Error = feedErr.Error()
		} else if tab == "sites" {
			data.Feed = a.siteRows(feed)
		} else {
			data.Feed = feed
		}
	}
	if tab != "" {
		data.Title = tab + " | " + a.backend.Slug()
	}
	a.render(writer, request, data)
}

var supportedMethods = []string{
	"supportedmethods", "stats", "getpolicy", "setpolicy", "listaudit", "listviews", "listmembers", "listpeople", "setmember", "allowpubkey", "unrulepubkey", "removemember", "createinvite", "listinvites", "revokeinvite", "listclaims", "createclaim", "deleteclaim", "removesubtree", "listbannedpubkeys", "listallowedpubkeys", "banpubkey", "listreports", "resolvereport", "banevent", "allowevent", "listeventsneedingmoderation", "blockip", "unblockip", "listblockedips", "exportconfig", "importconfig", "planconfig", "listblobs", "deleteblob", "listbannedevents", "deleteevent", "listrecentevents", "searchevents", "pinevent", "unpinevent", "listpins", "allowkind", "disallowkind", "unrulekind", "storagestats", "gitstorage", "setretention", "listretention", "purgekind", "listallowedkinds", "listblockedkinds", "notifytest", "resetrules", "listpresets", "listconnectiontemplates", "listconnections", "setconnections", "applypreset", "forkrelay", "pullfrom", "pullstatus", "listjobs", "deliverystatus", "addjob", "removejob", "runjob", "backfill", "transferowner", "listdumps", "deletedump", "dumpnow", "backupnow", "listbackups", "deletebackup", "changerelayname", "changerelaydescription", "changerelayicon", "adddomain", "setdomainsite", "checkdomain", "removedomain", "listdomains",
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
	switch path {
	case "manage":
		return "owner"
	case "repos":
		return "repos"
	case "repo":
		return "repo"
	case "files":
		return "files"
	case "file":
		return "file"
	case "tools", "signin", "sites":
		return path
	case "people", "moderation", "rules", "identity", "connect", "data", "sync", "views", "health", "owner":
		return path
	case "inbox", "outbox", "private", "chat", "media", "search", "articles", "dm", "quiet", "site", "marmot", "grasp", "terms":
		return path
	default:
		return "home"
	}
}

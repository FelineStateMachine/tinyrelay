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
}

func New(backend Backend, options Options) (*App, error) {
	if backend == nil {
		return nil, fmt.Errorf("webui backend is required")
	}
	page := guidedPageTemplate() + repoTemplate
	tmpl, err := template.New("webui").Funcs(template.FuncMap{
		"json": func(value any) string {
			encoded, encodeErr := json.Marshal(value)
			if encodeErr != nil {
				return "null"
			}
			return string(encoded)
		},
		"join":     strings.Join,
		"urlquery": url.QueryEscape,
		"asMap": func(value any) map[string]any {
			if result, ok := value.(map[string]any); ok {
				return result
			}
			encoded, _ := json.Marshal(value)
			var result map[string]any
			_ = json.Unmarshal(encoded, &result)
			return result
		},
		"lineRows": func(value any) []sourceLine {
			m := valueMap(value)
			content, _ := m["content"].(string)
			parts := strings.Split(content, "\n")
			rows := make([]sourceLine, 0, len(parts))
			for number, text := range parts {
				rows = append(rows, sourceLine{Number: number + 1, Text: text})
			}
			return rows
		},
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
	}).Parse(page)
	if err != nil {
		return nil, fmt.Errorf("parse web templates: %w", err)
	}
	qr := options.QR
	if qr == nil {
		qr = defaultQR
	}
	return &App{backend: backend, actor: options.Actor, qr: qr, tmpl: tmpl}, nil
}

type sourceLine struct {
	Number int
	Text   string
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
	encoded, _ := json.Marshal(result)
	var rows []any
	_ = json.Unmarshal(encoded, &rows)
	var out strings.Builder
	out.WriteString(`<thead><tr><th>Job</th><th>Status</th><th>Updated</th></tr></thead><tbody>`)
	for _, row := range rows {
		values, _ := row.(map[string]any)
		out.WriteString(`<tr><td>` + template.HTMLEscapeString(fmt.Sprint(nestedValue(values, "spec", "id"))) + `</td><td>` + template.HTMLEscapeString(fmt.Sprint(firstValue(values, "phase", "status"))) + `</td><td>` + template.HTMLEscapeString(fmt.Sprint(firstValue(values, "finished", "started"))) + `</td></tr>`)
	}
	out.WriteString(`</tbody>`)
	return out.String()
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
	encoded, _ := json.Marshal(result)
	var rows []any
	_ = json.Unmarshal(encoded, &rows)
	var out strings.Builder
	out.WriteString(`<table id="connections-preview"><thead><tr><th>Label</th><th>Relay</th><th>Status</th></tr></thead><tbody>`)
	for _, row := range rows {
		values, _ := row.(map[string]any)
		out.WriteString(`<tr><td>` + template.HTMLEscapeString(fmt.Sprint(firstValue(values, "label", "title", "name", "template"))) + `</td><td>` + template.HTMLEscapeString(fmt.Sprint(firstValue(values, "url", "href", "relay"))) + `</td><td>` + template.HTMLEscapeString(fmt.Sprint(firstValue(values, "status", "visibility"))) + `</td></tr>`)
	}
	out.WriteString(`</tbody></table>`)
	writer.Header().Set("content-type", "text/html; charset=utf-8")
	_, _ = writer.Write([]byte(out.String()))
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
	a.renderData(writer, request, PageData{Event: result, View: identifier, Tab: "event"})
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
		if view == "" {
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
	result, err := a.backend.Query(request.Context(), method, params, actor)
	if err != nil {
		a.renderData(writer, request, PageData{Tab: browseTab(path), Error: err.Error(), Query: request.URL.Query()})
		return
	}
	pageQuery := request.URL.Query()
	if method == "browserepo" && pageQuery.Get("view") == "" {
		pageQuery.Set("view", "tree")
	}
	data := PageData{Tab: browseTab(path), Feed: browseRows(result), Event: result, Query: pageQuery}
	a.renderData(writer, request, data)
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
	a.renderData(writer, request, PageData{Invite: code, Tab: "invite"})
}

func (a *App) renderData(writer http.ResponseWriter, request *http.Request, data PageData) {
	actor, _ := a.resolveActor(request)
	if data.Title == "" {
		data.Title = a.backend.Slug()
	}
	data.URL, data.Slug, data.Identity, data.Policy, data.Actor, data.Base = a.backend.URL(), a.backend.Slug(), a.backend.Identity(), a.backend.Policy(), actor, requestPrefix(request)
	data.Owner = actor != "" && actor == data.Policy.Owner
	data.Methods = supportedMethods
	a.privatePageData(&data, request, actor)
	writer.Header().Set("Cache-Control", "private, no-store")
	writer.Header().Set("content-type", "text/html; charset=utf-8")
	var rendered bytes.Buffer
	if err := a.tmpl.ExecuteTemplate(&rendered, "page", data); err != nil {
		http.Error(writer, "render page: "+err.Error(), http.StatusInternalServerError)
		return
	}
	html := strings.Replace(rendered.String(), "</head>", "<script src=\"/fixi.js\"></script><script src=\"/signer.js\"></script><script src=\"/webmcp.js\"></script></head>", 1)
	html = strings.Replace(html, "</body>", "<script>"+signerBridgeJS+"</script></body>", 1)
	html = injectBase(html, data.Base)
	_, _ = writer.Write([]byte(html))
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
	data := PageData{Title: a.backend.Slug(), URL: a.backend.URL(), Slug: a.backend.Slug(), Identity: a.backend.Identity(), Policy: a.backend.Policy(), Actor: actor, Tab: tab, Methods: supportedMethods, Base: requestPrefix(request)}
	data.Query = request.URL.Query()
	data.Owner = actor != "" && actor == data.Policy.Owner
	a.privatePageData(&data, request, actor)
	writer.Header().Set("Cache-Control", "private, no-store")
	if tab == "inbox" || tab == "outbox" || tab == "search" || tab == "articles" {
		feed, feedErr := a.publicFeed(request.Context(), tab, actor, request.URL.Query())
		if feedErr != nil {
			data.Error = feedErr.Error()
		} else {
			data.Feed = feed
		}
	}
	if tab != "" {
		data.Title = tab + " | " + data.Slug
	}
	writer.Header().Set("content-type", "text/html; charset=utf-8")
	var rendered bytes.Buffer
	if err := a.tmpl.ExecuteTemplate(&rendered, "page", data); err != nil {
		http.Error(writer, "render page: "+err.Error(), http.StatusInternalServerError)
		return
	}
	html := strings.Replace(rendered.String(), "</head>", "<script src=\"/fixi.js\"></script><script src=\"/signer.js\"></script><script src=\"/webmcp.js\"></script></head>", 1)
	html = strings.Replace(html, "</body>", "<script>"+signerBridgeJS+"</script></body>", 1)
	html = injectBase(html, data.Base)
	_, _ = writer.Write([]byte(html))
}

const signerBridgeJS = `(()=>{
const root=(location.pathname.match(/^\/r\/[^/]+/)||[""])[0],localPath=p=>p&&p.startsWith("/")?(p===root||p.startsWith(root+"/")?p:root+p):p;
const status=document.getElementById("signer-status")||document.getElementById("session-status"), qr=document.getElementById("signer-qr"),storagePrefix="tiny.bunker"+(root||"/"),storageURI=storagePrefix+".uri",storageKey=storagePrefix+".sk",storagePubkey=storagePrefix+".identity",storageRemote=storagePrefix+".remote";
const say=(text)=>{if(status)status.textContent=text};
const random=()=>{const b=new Uint8Array(32);crypto.getRandomValues(b);return [...b].map(x=>x.toString(16).padStart(2,"0")).join("")};
const signedSession=async(path,method="POST")=>{path=localPath(path);if(!window.nostr?.signEvent)throw Error("Connect a Nostr signer first.");const body="";const digest=await crypto.subtle.digest("SHA-256",new TextEncoder().encode(body));const hash=Array.from(new Uint8Array(digest),b=>b.toString(16).padStart(2,"0")).join("");const url=new URL(path,location.href).href;const event=await window.nostr.signEvent({kind:27235,created_at:Math.floor(Date.now()/1000),tags:[["u",url],["method",method],["payload",hash]],content:""});const response=await fetch(path,{method,headers:{authorization:"Nostr "+btoa(JSON.stringify(event))}});if(!response.ok)throw Error(await response.text());return response};
const signedFetch=async(path,method,body,options={})=>{const root=(location.pathname.match(/^\/r\/[^/]+/)||[""])[0];if(path.startsWith("/")&&! (path===root||path.startsWith(root+"/")))path=root+path;if(!window.nostr?.signEvent)throw Error("Connect a Nostr signer first.");const bytes=body instanceof ArrayBuffer?new Uint8Array(body):body instanceof Uint8Array?body:new TextEncoder().encode(body||"");const digest=await crypto.subtle.digest("SHA-256",bytes);const hash=Array.from(new Uint8Array(digest),b=>b.toString(16).padStart(2,"0")).join("");const url=new URL(path,location.href).href;const event=await window.nostr.signEvent({kind:27235,created_at:Math.floor(Date.now()/1000),tags:[["u",url],["method",method],["payload",hash]],content:""});const request={method,signal:options.signal,headers:{authorization:"Nostr "+btoa(JSON.stringify(event)),"content-type":options.contentType||"application/octet-stream"}};if(method!=="GET"&&method!=="HEAD")request.body=bytes;const response=await fetch(path,request);if(!response.ok)throw Error(await response.text());return response};window.tinySignedFetch=signedFetch;
const signIn=async()=>{say("Signing in…");await signedSession("/session");say("Signed in.");location.assign(localPath("/"))};
const login=document.getElementById("session-login");if(login)login.addEventListener("click",async()=>{try{await signIn()}catch(err){say("Sign-in error: "+err.message)}});
const logout=document.getElementById("session-logout");if(logout)logout.addEventListener("click",async()=>{try{say("Signing out…");const response=await fetch(localPath("/session/logout"),{method:"POST",credentials:"same-origin"});if(!response.ok)throw Error(await response.text());sessionStorage.removeItem(storageURI);sessionStorage.removeItem(storageKey);sessionStorage.removeItem(storagePubkey);sessionStorage.removeItem(storageRemote);sessionStorage.removeItem("tiny.bunker");say("Signed out.");location.reload()}catch(err){say("Sign-out error: "+err.message)}});
const nc=document.getElementById("nostrconnect");
const setSigner=(signer)=>{window.tinySigner=signer;window.nostr={signEvent:e=>window.tinySigner.signEvent(e)};if(document.getElementById("signer-status"))say(document.getElementById("session-logout")?"Signer connected.":"Signer connected. Choose Sign in with connected signer to continue.")};
if(nc)nc.addEventListener("click",async ev=>{ev.preventDefault();const key=window.NostrSigner.generateSecretKey();const uri=window.NostrSigner.createNostrConnectURI({clientPubkey:window.NostrSigner.getPublicKey(key),relays:[location.origin.replace(/^http/,"ws")+root],secret:random(),name:"tiny",url:location.origin,perms:["sign_event:27235"]});const open=document.getElementById("nostrconnect-open");if(open){open.href=uri;open.hidden=false}if(qr){qr.src=root+"/qr.svg?text="+encodeURIComponent(uri);qr.hidden=false}navigator.clipboard?.writeText(uri).catch(()=>{});say("Scan the QR code or open the link in your signer.");window.tinyConnect={key,uri};try{const signer=await window.NostrSigner.BunkerSigner.fromURI(key,uri);setSigner(signer);sessionStorage.setItem(storageURI,uri);sessionStorage.setItem(storageKey,window.NostrSigner.bytesToHex(key));sessionStorage.setItem(storagePubkey,await signer.getPublicKey());sessionStorage.setItem(storageRemote,JSON.stringify(signer.bp));await signIn()}catch(err){say("Nostr Connect error: "+err.message)}});
const bunker=document.getElementById("bunker");
if(bunker)bunker.addEventListener("submit",async ev=>{ev.preventDefault();const value=document.getElementById("bunker-url").value.trim();if(!value)return;try{const secret=window.NostrSigner.generateSecretKey();const bp=await window.NostrSigner.parseBunkerInput(value);const signer=window.NostrSigner.BunkerSigner.fromBunker(secret,bp);await signer.connect();const signerPubkey=await signer.getPublicKey();setSigner(signer);sessionStorage.setItem(storageURI,value);sessionStorage.setItem(storageKey,window.NostrSigner.bytesToHex(secret));sessionStorage.setItem(storagePubkey,signerPubkey);sessionStorage.setItem(storageRemote,JSON.stringify(signer.bp));await signIn()}catch(err){say("Remote signer error: "+err.message)}});const resume=async()=>{const uri=sessionStorage.getItem(storageURI),hex=sessionStorage.getItem(storageKey),expected=sessionStorage.getItem(storagePubkey),remote=sessionStorage.getItem(storageRemote);if(!uri||!hex)return;try{const key=window.NostrSigner.hexToBytes(hex);let signer;if(remote){signer=window.NostrSigner.BunkerSigner.fromBunker(key,JSON.parse(remote));await signer.connect();}else{const bp=await window.NostrSigner.parseBunkerInput(uri);signer=window.NostrSigner.BunkerSigner.fromBunker(key,bp);await signer.connect();}const pubkey=await signer.getPublicKey();if(expected&&pubkey!==expected)throw Error("Remote signer identity changed");setSigner(signer)}catch(err){sessionStorage.removeItem(storageURI);sessionStorage.removeItem(storageKey);sessionStorage.removeItem(storagePubkey);sessionStorage.removeItem(storageRemote);say("Remote signer resume error: "+err.message)}};resume();
const dataUpload=(id,input,method,path)=>{const form=document.getElementById(id);if(!form)return;form.addEventListener("submit",async ev=>{ev.preventDefault();const file=document.getElementById(input).files[0],out=document.getElementById("data-status");if(!file){if(out)out.textContent="Choose a file first.";return}try{if(out)out.textContent="Signing…";const response=await signedFetch(path,method,await file.arrayBuffer());if(out)out.textContent="Done: "+await response.text()}catch(err){if(out)out.textContent="Error: "+err.message}})};dataUpload("import-file","import-input","PUT","/import");const blobUpload=document.getElementById("blob-upload");if(blobUpload)blobUpload.addEventListener("submit",async ev=>{ev.preventDefault();const file=document.getElementById("blob-upload-input").files[0],out=blobUpload.querySelector("output");if(!file)return;try{out.textContent="Uploading…";const response=await signedFetch("/upload","PUT",await file.arrayBuffer());out.textContent="Uploaded: "+await response.text()}catch(err){out.textContent="Error: "+err.message}});dataUpload("backup-preview","backup-preview-input","POST","/backups/preview");dataUpload("backup-restore","backup-restore-input","POST","/backups/restore");const downloadFile=(id,name,path)=>{const form=document.getElementById(id);if(!form)return;form.addEventListener("submit",async ev=>{ev.preventDefault();const value=document.getElementById(name).value.trim();if(!value)return;try{const r=await signedFetch(path+encodeURIComponent(value),"GET",new Uint8Array()),blob=await r.blob(),url=URL.createObjectURL(blob),a=document.createElement("a");a.href=url;a.download=value;a.click();setTimeout(()=>URL.revokeObjectURL(url),1000)}catch(err){const out=document.getElementById("data-status");if(out)out.textContent="Error: "+err.message}})};downloadFile("backup-download","backup-download-name","/backups/");downloadFile("dump-download","dump-download-name","/dumps/");
})();`

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

func injectBase(html, base string) string {
	if base == "" {
		return html
	}
	for _, attr := range []string{"href", "src", "fx-action", "action"} {
		html = strings.ReplaceAll(html, " "+attr+"=\"/", " "+attr+"=\""+base+"/")
	}
	html = strings.ReplaceAll(html, "fetch(\"/", "fetch(\""+base+"/")
	html = strings.ReplaceAll(html, "new URL(\"/", "new URL(\""+base+"/")
	html = strings.ReplaceAll(html, "signedSession(\"/", "signedSession(\""+base+"/")
	html = strings.ReplaceAll(html, "qr.src=\"/", "qr.src=\""+base+"/")
	return html
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
	case "tools", "signin":
		return path
	case "people", "moderation", "rules", "identity", "connect", "data", "sync", "views", "health", "owner":
		return path
	case "inbox", "outbox", "private", "chat", "media", "search", "articles", "dm", "quiet", "site", "marmot", "grasp", "terms":
		return path
	default:
		return "home"
	}
}

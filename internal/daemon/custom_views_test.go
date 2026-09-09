package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/views"
	"github.com/FelineStateMachine/tinyrelay/internal/work"
)

const (
	testSVG    = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10"/></svg>`
	testPNGRaw = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"
)

// transformServer stands in for a view transform: it records every request
// and answers with the artifacts it is told to.
type transformServer struct {
	mu       sync.Mutex
	status   int
	body     string
	headers  []http.Header
	requests [][]byte
}

func (s *transformServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.headers = append(s.headers, r.Header.Clone())
	s.requests = append(s.requests, body)
	status, answer := s.status, s.body
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(answer))
}

func (s *transformServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *transformServer) answer(status int, artifacts ...map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	raw, _ := json.Marshal(map[string]any{"artifacts": artifacts, "errors": []map[string]any{{"block": 9, "error": "unknown"}}})
	s.body = string(raw)
}

func svgArtifact(block int) map[string]any {
	return map[string]any{"block": block, "type": "image/svg+xml", "body": testSVG, "engine": "mermaid"}
}

func viewParams(values map[string]any) []json.RawMessage {
	raw, _ := json.Marshal(values)
	return []json.RawMessage{raw}
}

func addView(t *testing.T, tenant *Tenant, values map[string]any) map[string]any {
	t.Helper()
	result, err := tenant.Execute(context.Background(), tenant.Policy().Owner, "addcustomview", viewParams(values))
	if err != nil {
		t.Fatalf("addcustomview: %v", err)
	}
	return result.(map[string]any)
}

// viewTenant is a loopback tenant with a transform server and one view that
// watches the given kinds.
func viewTenant(t *testing.T, kinds []int, extra map[string]any) (*Tenant, *transformServer, map[string]any) {
	t.Helper()
	_, tenant := loopbackTenant(t)
	server := &transformServer{}
	server.answer(http.StatusOK)
	service := httptest.NewTLSServer(server)
	t.Cleanup(service.Close)
	tenant.viewClient = service.Client()
	values := map[string]any{"name": "diagrams", "kinds": kinds, "transform": service.URL + "/render?token=t0k3n", "languages": []string{"mermaid", "dot"}}
	for key, value := range extra {
		values[key] = value
	}
	return tenant, server, addView(t, tenant, values)
}

// pendingViewIntents lists queued transforms of one source for one view,
// first attempt first.
func pendingViewIntents(t *testing.T, tenant *Tenant, name, eventID string) []work.Intent {
	t.Helper()
	rows, err := tenant.store.DB().QueryContext(context.Background(), "SELECT id,event_id,payload,next_at FROM work_intents WHERE kind=? AND target=? AND state='pending' AND (event_id=? OR event_id LIKE ? OR event_id LIKE ?) ORDER BY next_at,created_at,id", viewTransform, name, eventID, eventID+"#%", eventID+"@%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []work.Intent
	for rows.Next() {
		var item work.Intent
		var next int64
		if err := rows.Scan(&item.ID, &item.EventID, &item.Payload, &next); err != nil {
			t.Fatal(err)
		}
		item.Kind, item.Target, item.NextAt = viewTransform, name, time.Unix(next, 0)
		out = append(out, item)
	}
	return out
}

// runViewIntent drives one intent the way the worker would and marks it
// complete.
func runViewIntent(t *testing.T, tenant *Tenant, intent work.Intent) {
	t.Helper()
	if err := tenant.handleViewTransform(context.Background(), intent); err != nil {
		t.Fatalf("handleViewTransform: %v", err)
	}
	if _, err := tenant.store.DB().ExecContext(context.Background(), "UPDATE work_intents SET state='completed' WHERE id=?", intent.ID); err != nil {
		t.Fatal(err)
	}
}

func runPendingView(t *testing.T, tenant *Tenant, name, eventID string) {
	t.Helper()
	queued := pendingViewIntents(t, tenant, name, eventID)
	if len(queued) != 1 {
		t.Fatalf("queued intents for %s = %d", eventID, len(queued))
	}
	runViewIntent(t, tenant, queued[0])
}

func storedArtifact(t *testing.T, tenant *Tenant, name, hash string) (event.Event, bool) {
	t.Helper()
	var raw string
	err := tenant.store.DB().QueryRowContext(context.Background(), "SELECT raw FROM events WHERE kind=? AND pubkey=? AND d=?", event.KIND_VIEW, tenant.records.PublicKey(), "bind.ws/view/"+name+"/"+hash).Scan(&raw)
	if err != nil {
		return event.Event{}, false
	}
	e, err := event.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return e, true
}

func artifactTags(e event.Event, name string) []string {
	var out []string
	for _, tag := range e.Tags {
		if len(tag) >= 2 && tag[0] == name {
			out = append(out, tag[1])
		}
	}
	return out
}

func signRequestAs(t *testing.T, r *http.Request, secret string) {
	t.Helper()
	hash := sha256.Sum256(nil)
	e := event.Event{Kind: 27235, CreatedAt: time.Now().Unix(), Tags: [][]string{{"u", r.URL.String()}, {"method", r.Method}, {"payload", hex.EncodeToString(hash[:])}}}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(e)
	r.Header.Set("Authorization", "Nostr "+base64.StdEncoding.EncodeToString(raw))
}

func getArtifact(t *testing.T, tenant *Tenant, path, secret string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080"+path, nil)
	if secret != "" {
		signRequestAs(t, req, secret)
	}
	res := httptest.NewRecorder()
	tenant.ServeHTTP(res, req)
	return res
}

func TestCustomViewManagementIsOwnerOnlyAndValidated(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	member, _ := event.PublicKey(testMemberSecret)
	moderator, _ := event.PublicKey(testModSecret)
	guest := strings.Repeat("e", 64)
	setRole(t, tenant, member, "member")
	setRole(t, tenant, moderator, "moderator")
	methods, err := tenant.Execute(ctx, owner, "supportedmethods", nil)
	if err != nil {
		t.Fatal(err)
	}
	all := []string{"listcustomviews", "addcustomview", "removecustomview", "runcustomview", "pausecustomview", "resumecustomview"}
	for _, name := range all {
		if !containsString(methods.([]string), name) || !containsString(ManagementMethods(), name) {
			t.Fatalf("registry lacks %s", name)
		}
	}
	base := map[string]any{"name": "diagrams", "kinds": []int{1, 30023}, "transform": "https://render.example/tiny", "languages": []string{"mermaid"}}
	for _, actor := range []string{guest, member, moderator} {
		for _, method := range all {
			params := []json.RawMessage{json.RawMessage(`"diagrams"`)}
			if method == "addcustomview" {
				params = viewParams(base)
			}
			if _, err := tenant.Execute(ctx, actor, method, params); err == nil || !strings.HasPrefix(err.Error(), "restricted:") {
				t.Fatalf("%s by non-owner: %v", method, err)
			}
		}
	}
	with := func(changes map[string]any) map[string]any {
		out := map[string]any{}
		for key, value := range base {
			out[key] = value
		}
		for key, value := range changes {
			out[key] = value
		}
		return out
	}
	many := make([]int, viewKindMax+1)
	for _, tc := range []struct {
		name   string
		values map[string]any
		want   string
	}{
		{"bad name", with(map[string]any{"name": "Diagrams!"}), "invalid: name"},
		{"long name", with(map[string]any{"name": strings.Repeat("a", 33)}), "invalid: name"},
		{"no kinds", with(map[string]any{"kinds": []int{}}), "invalid: kinds"},
		{"too many kinds", with(map[string]any{"kinds": many}), "invalid: kinds"},
		{"bad kind", with(map[string]any{"kinds": []int{70000}}), "invalid: kinds must be between"},
		{"http transform", with(map[string]any{"transform": "http://render.example/tiny"}), "invalid: transform"},
		{"private transform", with(map[string]any{"transform": "https://10.0.0.1/tiny"}), "invalid: transform"},
		{"bad trigger", with(map[string]any{"trigger": "daily"}), "invalid: trigger"},
		{"bad audience", with(map[string]any{"audience": "owner"}), "invalid: audience"},
		{"no languages", with(map[string]any{"languages": []string{}}), "invalid: languages"},
		{"bad language", with(map[string]any{"languages": []string{"mer maid"}}), "invalid: languages"},
		{"max bytes cap", with(map[string]any{"max_bytes": viewMaxBytesCap + 1}), "invalid: max_bytes"},
		{"zero max bytes", with(map[string]any{"max_bytes": 0}), "invalid: max_bytes"},
		{"short secret", with(map[string]any{"secret": "short"}), "invalid: secret"},
		{"control in secret", with(map[string]any{"secret": strings.Repeat("a", 15) + "\n"}), "invalid: secret"},
	} {
		if _, err := tenant.Execute(ctx, owner, "addcustomview", viewParams(tc.values)); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want prefix %q", tc.name, err, tc.want)
		}
	}
	// A plain form sends kinds and languages as text; defaults fill trigger,
	// audience and the size limit; the secret is generated and shown once.
	added := addView(t, tenant, map[string]any{"name": "diagrams", "kinds": "30023, 1, 1", "transform": "https://render.example/tiny", "languages": "Mermaid, dot"})
	secret, _ := added["secret"].(string)
	if len(secret) != 64 || added["trigger"] != "write" || added["audience"] != "public" || added["maxBytes"] != viewDefaultMaxBytes || added["host"] != "render.example" {
		t.Fatalf("added = %v", added)
	}
	if kinds, _ := json.Marshal(added["kinds"]); string(kinds) != "[1,30023]" {
		t.Fatalf("kinds = %s", kinds)
	}
	if languages, _ := json.Marshal(added["languages"]); string(languages) != `["mermaid","dot"]` {
		t.Fatalf("languages = %s", languages)
	}
	if _, err := tenant.Execute(ctx, owner, "addcustomview", viewParams(base)); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("duplicate name: %v", err)
	}
	// An owner may paste the secret the transform already holds.
	pasted := addView(t, tenant, with(map[string]any{"name": "charts", "trigger": "hourly", "audience": "members", "max_bytes": 2048, "secret": "shared-secret-value-1234"}))
	if pasted["secret"] != "shared-secret-value-1234" || pasted["trigger"] != "hourly" || pasted["audience"] != "members" || pasted["maxBytes"] != 2048 {
		t.Fatalf("pasted = %v", pasted)
	}
	var stored string
	if err := tenant.store.DB().QueryRowContext(ctx, "SELECT secret FROM custom_views WHERE name='charts'").Scan(&stored); err != nil || stored != "shared-secret-value-1234" {
		t.Fatalf("stored secret %q %v", stored, err)
	}
	listed, err := tenant.Execute(ctx, owner, "listcustomviews", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := listed.([]map[string]any)
	if len(rows) != 2 || rows[0]["name"] != "diagrams" || rows[1]["name"] != "charts" {
		t.Fatalf("listing = %v", rows)
	}
	for _, row := range rows {
		if _, leaked := row["secret"]; leaked || row["state"] != "active" {
			t.Fatalf("listing row = %v", row)
		}
	}
	paused, err := tenant.Execute(ctx, owner, "pausecustomview", []json.RawMessage{json.RawMessage(`{"name":"diagrams"}`)})
	if err != nil || paused.(map[string]any)["enabled"] != false || paused.(map[string]any)["state"] != "paused" {
		t.Fatalf("pause: %v %v", paused, err)
	}
	if _, err := tenant.Execute(ctx, owner, "runcustomview", []json.RawMessage{json.RawMessage(`"diagrams"`)}); err == nil || !strings.Contains(err.Error(), "resume") {
		t.Fatalf("run while paused: %v", err)
	}
	for _, view := range tenant.customViewIndex(ctx)[1] {
		if view.Name == "diagrams" {
			t.Fatal("paused view stayed in the kind index")
		}
	}
	resumed, err := tenant.Execute(ctx, owner, "resumecustomview", []json.RawMessage{json.RawMessage(`"diagrams"`)})
	if err != nil || resumed.(map[string]any)["enabled"] != true {
		t.Fatalf("resume: %v %v", resumed, err)
	}
	ran, err := tenant.Execute(ctx, owner, "runcustomview", []json.RawMessage{json.RawMessage(`"diagrams"`)})
	if err != nil || ran.(map[string]any)["queued"] != 0 {
		t.Fatalf("run: %v %v", ran, err)
	}
	if _, err := tenant.Execute(ctx, owner, "removecustomview", []json.RawMessage{json.RawMessage(`"missing"`)}); err == nil || !strings.HasPrefix(err.Error(), "not found:") {
		t.Fatalf("remove missing: %v", err)
	}
	removed, err := tenant.Execute(ctx, owner, "removecustomview", []json.RawMessage{json.RawMessage(`"charts"`)})
	if err != nil || removed.(map[string]any)["removed"] != true {
		t.Fatalf("remove: %v %v", removed, err)
	}
	audit, err := tenant.Execute(ctx, owner, "listaudit", nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range audit.([]community.AuditRow) {
		seen[row.Action] = true
		if strings.Contains(row.Detail, "render.example") || strings.Contains(row.Target, "render.example") || strings.Contains(row.Detail, secret) {
			t.Fatalf("audit carries a transform URL or secret: %+v", row)
		}
	}
	for _, action := range all {
		if action != "listcustomviews" && !seen[action] {
			t.Fatalf("audit lacks %s: %v", action, seen)
		}
	}
}

func TestCustomViewTransformSendsBlocksAndKeepsSignedArtifacts(t *testing.T) {
	tenant, server, added := viewTenant(t, []int{1}, nil)
	ctx := context.Background()
	secret := added["secret"].(string)
	server.answer(http.StatusOK, svgArtifact(0), map[string]any{"block": 2, "type": "image/png", "body": base64.StdEncoding.EncodeToString([]byte(testPNGRaw))}, map[string]any{"block": 1, "type": "image/svg+xml", "body": testSVG})
	now := time.Now().Unix()
	content := "Look:\n\n```mermaid\ngraph TD;\n  A-->B;\n```\n\n```go\nfunc main() {}\n```\n\n~~~ dot\ndigraph { a -> b }\n~~~\n"
	note := signedEvent(t, testOwnerSecret, 1, now, [][]string{{"expiration", strconv.FormatInt(now+86400, 10)}}, content)
	if err := publishAs(t, tenant, note); err != nil {
		t.Fatal(err)
	}
	plain := signedEvent(t, testOwnerSecret, 1, now+1, nil, "no blocks here\n```go\nx\n```")
	if err := publishAs(t, tenant, plain); err != nil {
		t.Fatal(err)
	}
	if len(pendingViewIntents(t, tenant, "diagrams", plain.ID)) != 0 {
		t.Fatal("an event without a matching block was queued")
	}
	runPendingView(t, tenant, "diagrams", note.ID)
	if server.count() != 1 {
		t.Fatalf("requests = %d", server.count())
	}
	headers, body := server.headers[0], server.requests[0]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if headers.Get("Content-Type") != "application/json" || headers.Get("X-Tiny-View") != "diagrams" || headers.Get("X-Tiny-Relay") != "http://127.0.0.1:8080" || headers.Get("X-Tiny-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("headers = %v", headers)
	}
	for _, field := range []string{"View", "Relay", "Signature"} {
		if headers.Get("X-Transform-"+field) != headers.Get("X-Tiny-"+field) {
			t.Fatalf("canonical transform header %s differs from its legacy value", field)
		}
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if _, leaked := request["event"]; leaked || request["relay"] != "http://127.0.0.1:8080" || request["view"] != "diagrams" {
		t.Fatalf("request = %s", body)
	}
	if source := request["source"].(map[string]any); source["id"] != note.ID || source["kind"] != float64(1) || len(source) != 2 {
		t.Fatalf("source = %v", source)
	}
	if strings.Contains(string(body), "Look:") || strings.Contains(string(body), tenant.Policy().Owner) {
		t.Fatalf("request carries event content or author: %s", body)
	}
	blocks := request["blocks"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %v", blocks)
	}
	first, third := blocks[0].(map[string]any), blocks[1].(map[string]any)
	if first["index"] != float64(0) || first["lang"] != "mermaid" || first["source"] != "graph TD;\n  A-->B;" || third["index"] != float64(2) || third["lang"] != "dot" || third["source"] != "digraph { a -> b }" {
		t.Fatalf("blocks = %v", blocks)
	}
	// The SVG artifact is a stored relay-signed record attached to the note.
	svgHash := views.Hash("mermaid", "graph TD;\n  A-->B;")
	artifact, ok := storedArtifact(t, tenant, "diagrams", svgHash)
	if !ok {
		t.Fatal("svg artifact was not stored")
	}
	if artifact.Content != testSVG || artifact.PubKey != tenant.records.PublicKey() || event.Tag(artifact, "view") != "diagrams" || event.Tag(artifact, "e") != note.ID || event.Tag(artifact, "block") != "0" || event.Tag(artifact, "type") != "image/svg+xml" || event.Tag(artifact, "engine") != "mermaid" || event.Tag(artifact, "expiration") != strconv.FormatInt(now+86400, 10) || len(artifact.Tags[0]) != 1 || artifact.Tags[0][0] != "-" {
		t.Fatalf("artifact = %+v", artifact)
	}
	pngHash := views.Hash("dot", "digraph { a -> b }")
	png, ok := storedArtifact(t, tenant, "diagrams", pngHash)
	if !ok || png.Content != base64.StdEncoding.EncodeToString([]byte(testPNGRaw)) || event.Tag(png, "type") != "image/png" || event.Tag(png, "block") != "2" || event.Tag(png, "engine") != "" {
		t.Fatalf("png artifact = %+v %v", png, ok)
	}
	var count int
	if err := tenant.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM custom_view_artifacts").Scan(&count); err != nil || count != 2 {
		t.Fatalf("artifacts = %d %v (an artifact for an unsent block was kept)", count, err)
	}
	var lastStatus string
	var failures int
	if err := tenant.store.DB().QueryRowContext(ctx, "SELECT last_status,failures FROM custom_views WHERE name='diagrams'").Scan(&lastStatus, &failures); err != nil || lastStatus != "ok" || failures != 0 {
		t.Fatalf("view state %q %d %v", lastStatus, failures, err)
	}

	// The route serves each artifact under a sandbox, by its own type.
	res := getArtifact(t, tenant, "/views/diagrams/"+svgHash+".svg", "")
	if res.Code != http.StatusOK || res.Body.String() != testSVG || res.Header().Get("Content-Type") != "image/svg+xml" || res.Header().Get("Content-Security-Policy") != "sandbox; default-src 'none'; style-src 'unsafe-inline'" || res.Header().Get("X-Content-Type-Options") != "nosniff" || res.Header().Get("Cache-Control") != "public, no-cache" || res.Header().Get("ETag") == "" {
		t.Fatalf("svg route: %d %v %s", res.Code, res.Header(), res.Body.String())
	}
	conditional := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/views/diagrams/"+svgHash+".svg", nil)
	conditional.Header.Set("If-None-Match", res.Header().Get("ETag"))
	conditionalResponse := httptest.NewRecorder()
	tenant.ServeHTTP(conditionalResponse, conditional)
	if conditionalResponse.Code != http.StatusNotModified {
		t.Fatalf("conditional artifact: %d %v", conditionalResponse.Code, conditionalResponse.Header())
	}
	res = getArtifact(t, tenant, "/views/diagrams/"+pngHash+".png", "")
	if res.Code != http.StatusOK || res.Body.String() != testPNGRaw || res.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("png route: %d %v", res.Code, res.Header())
	}
	for _, path := range []string{"/views/diagrams/" + pngHash + ".svg", "/views/diagrams/" + strings.Repeat("0", 64) + ".svg", "/views/other/" + svgHash + ".svg", "/views/diagrams/" + svgHash + ".gif", "/views/diagrams/" + svgHash + ".", "/views/diagrams"} {
		if res := getArtifact(t, tenant, path, ""); res.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", path, res.Code)
		}
	}

	// A second note with the same block reuses the artifact: no POST, a
	// fresh record with both sources.
	second := signedEvent(t, testOwnerSecret, 1, now+2, nil, "Again:\n\n```mermaid\ngraph TD;\n  A-->B;\n```\n")
	if err := publishAs(t, tenant, second); err != nil {
		t.Fatal(err)
	}
	runPendingView(t, tenant, "diagrams", second.ID)
	if server.count() != 1 {
		t.Fatalf("requests after reuse = %d", server.count())
	}
	artifact, _ = storedArtifact(t, tenant, "diagrams", svgHash)
	if sources := artifactTags(artifact, "e"); len(sources) != 2 || sources[0] != note.ID || sources[1] != second.ID {
		t.Fatalf("sources after reuse = %v", sources)
	}
	if event.Tag(artifact, "expiration") != "" {
		t.Fatal("artifact kept an expiration while a source without one is attached")
	}
	if blocks := artifactTags(artifact, "block"); len(blocks) != 2 || blocks[0] != "0" || blocks[1] != "0" {
		t.Fatalf("block tags = %v", blocks)
	}

	// Deleting the first note detaches it; deleting the second removes the
	// artifact and its record.
	deletion := signedEvent(t, testOwnerSecret, event.KIND_DELETION, now+3, [][]string{{"e", note.ID}}, "")
	if err := publishAs(t, tenant, deletion); err != nil {
		t.Fatal(err)
	}
	artifact, ok = storedArtifact(t, tenant, "diagrams", svgHash)
	if !ok || len(artifactTags(artifact, "e")) != 1 || event.Tag(artifact, "e") != second.ID {
		t.Fatalf("artifact after first deletion = %+v %v", artifact, ok)
	}
	if _, ok := storedArtifact(t, tenant, "diagrams", pngHash); ok {
		t.Fatal("png artifact survived the deletion of its only source")
	}
	if res := getArtifact(t, tenant, "/views/diagrams/"+pngHash+".png", ""); res.Code != http.StatusNotFound {
		t.Fatalf("png route after deletion: %d", res.Code)
	}
	deletion = signedEvent(t, testOwnerSecret, event.KIND_DELETION, now+4, [][]string{{"e", second.ID}}, "")
	if err := publishAs(t, tenant, deletion); err != nil {
		t.Fatal(err)
	}
	if _, ok := storedArtifact(t, tenant, "diagrams", svgHash); ok {
		t.Fatal("svg artifact survived the deletion of its last source")
	}
	if err := tenant.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM custom_view_artifacts").Scan(&count); err != nil || count != 0 {
		t.Fatalf("artifacts after deletions = %d %v", count, err)
	}

	// Expiry: an expired source loses its artifacts at the sweep.
	expiring := signedEvent(t, testOwnerSecret, 1, now+5, [][]string{{"expiration", strconv.FormatInt(now+30, 10)}}, "```dot\nx\n```")
	if err := publishAs(t, tenant, expiring); err != nil {
		t.Fatal(err)
	}
	server.answer(http.StatusOK, svgArtifact(0))
	runPendingView(t, tenant, "diagrams", expiring.ID)
	expiringHash := views.Hash("dot", "x")
	if _, ok := storedArtifact(t, tenant, "diagrams", expiringHash); !ok {
		t.Fatal("expiring artifact was not stored")
	}
	if err := tenant.sweep(ctx, now+60); err != nil {
		t.Fatal(err)
	}
	if _, ok := storedArtifact(t, tenant, "diagrams", expiringHash); ok {
		t.Fatal("artifact survived its source's expiry")
	}
	if res := getArtifact(t, tenant, "/views/diagrams/"+expiringHash+".svg", ""); res.Code != http.StatusNotFound {
		t.Fatalf("expired route: %d", res.Code)
	}

	// Removing the view removes what is left.
	last := signedEvent(t, testOwnerSecret, 1, now+6, nil, "```dot\ny\n```")
	if err := publishAs(t, tenant, last); err != nil {
		t.Fatal(err)
	}
	runPendingView(t, tenant, "diagrams", last.ID)
	if _, err := tenant.Execute(ctx, tenant.Policy().Owner, "removecustomview", []json.RawMessage{json.RawMessage(`"diagrams"`)}); err != nil {
		t.Fatal(err)
	}
	if err := tenant.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM events WHERE kind=? AND pubkey=?", event.KIND_VIEW, tenant.records.PublicKey()).Scan(&count); err != nil || count != 0 {
		t.Fatalf("records after removal = %d %v", count, err)
	}
}

func TestCustomViewReplacementReusesAndDropsBlocks(t *testing.T) {
	tenant, server, _ := viewTenant(t, []int{30023}, nil)
	ctx := context.Background()
	now := time.Now().Unix()
	server.answer(http.StatusOK, svgArtifact(0), svgArtifact(1))
	first := signedEvent(t, testOwnerSecret, 30023, now, [][]string{{"d", "post"}}, "```mermaid\nA\n```\n\n```mermaid\nB\n```\n")
	if err := publishAs(t, tenant, first); err != nil {
		t.Fatal(err)
	}
	runPendingView(t, tenant, "diagrams", first.ID)
	hashA, hashB := views.Hash("mermaid", "A"), views.Hash("mermaid", "B")
	if _, ok := storedArtifact(t, tenant, "diagrams", hashA); !ok {
		t.Fatal("A missing")
	}
	if _, ok := storedArtifact(t, tenant, "diagrams", hashB); !ok {
		t.Fatal("B missing")
	}
	// The replacement keeps A and drops B; A is reused without a POST and
	// carries only the new version.
	second := signedEvent(t, testOwnerSecret, 30023, now+1, [][]string{{"d", "post"}}, "```mermaid\nA\n```\n")
	if err := publishAs(t, tenant, second); err != nil {
		t.Fatal(err)
	}
	runPendingView(t, tenant, "diagrams", second.ID)
	if server.count() != 1 {
		t.Fatalf("requests = %d", server.count())
	}
	artifact, ok := storedArtifact(t, tenant, "diagrams", hashA)
	if !ok || len(artifactTags(artifact, "e")) != 1 || event.Tag(artifact, "e") != second.ID {
		t.Fatalf("A after replacement = %+v %v", artifact, ok)
	}
	if _, ok := storedArtifact(t, tenant, "diagrams", hashB); ok {
		t.Fatal("B survived the replacement")
	}
	// A stale intent for the replaced version renders nothing.
	stale := tenant.viewIntent(customView{Name: "diagrams"}, first, "old")
	if err := tenant.handleViewTransform(ctx, work.Intent{ID: "stale", Kind: stale.Kind, EventID: stale.EventID, Target: stale.Target, Payload: stale.Payload}); err != nil {
		t.Fatal(err)
	}
	if server.count() != 1 {
		t.Fatal("a replaced version was sent to the transform")
	}
	// Deleting the address removes the artifact.
	deletion := signedEvent(t, testOwnerSecret, event.KIND_DELETION, now+2, [][]string{{"a", "30023:" + tenant.Policy().Owner + ":post"}}, "")
	if err := publishAs(t, tenant, deletion); err != nil {
		t.Fatal(err)
	}
	if _, ok := storedArtifact(t, tenant, "diagrams", hashA); ok {
		t.Fatal("A survived the address deletion")
	}
}

func queueViewRebuild(t *testing.T, tenant *Tenant, source string) work.Intent {
	t.Helper()
	params := viewParams(map[string]any{"name": "diagrams", "refresh": true})
	if _, err := tenant.Execute(context.Background(), tenant.Policy().Owner, "runcustomview", params); err != nil {
		t.Fatal(err)
	}
	for _, intent := range pendingViewIntents(t, tenant, "diagrams", source) {
		var payload viewPayload
		if err := json.Unmarshal([]byte(intent.Payload), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Attempt == 1 {
			if !payload.Refresh {
				t.Fatal("queued rebuild lost refresh flag")
			}
			return intent
		}
	}
	t.Fatal("no rebuild was queued")
	return work.Intent{}
}

func TestCustomViewRebuildReplacesOnlyAfterSuccess(t *testing.T) {
	tenant, server, _ := viewTenant(t, []int{1}, nil)
	note := signedEvent(t, testOwnerSecret, 1, time.Now().Unix(), nil, "```mermaid\nA\n```")
	if err := publishAs(t, tenant, note); err != nil {
		t.Fatal(err)
	}
	server.answer(http.StatusOK, svgArtifact(0))
	runPendingView(t, tenant, "diagrams", note.ID)
	path := views.Path("diagrams", views.Hash("mermaid", "A"), "")
	before := getArtifact(t, tenant, path, "")
	if before.Code != http.StatusOK {
		t.Fatalf("initial artifact: %d", before.Code)
	}
	server.answer(http.StatusInternalServerError)
	runViewIntent(t, tenant, queueViewRebuild(t, tenant, note.ID))
	afterFailure := getArtifact(t, tenant, path, "")
	if afterFailure.Body.String() != before.Body.String() || afterFailure.Header().Get("ETag") != before.Header().Get("ETag") {
		t.Fatal("failed rebuild changed the stored artifact")
	}
	replacement := svgArtifact(0)
	replacement["body"] = strings.Replace(testSVG, "<rect", `<rect fill="#334155"`, 1)
	server.answer(http.StatusOK, replacement)
	runViewIntent(t, tenant, queueViewRebuild(t, tenant, note.ID))
	after := getArtifact(t, tenant, path, "")
	if after.Body.String() != replacement["body"] || after.Header().Get("ETag") == before.Header().Get("ETag") {
		t.Fatal("successful rebuild did not replace and revalidate the artifact")
	}
	if server.count() != 3 {
		t.Fatalf("transform requests = %d", server.count())
	}
}

func TestCustomViewTransformRetriesThenPauses(t *testing.T) {
	tenant, server, _ := viewTenant(t, []int{1}, nil)
	ctx := context.Background()
	now := time.Now().Unix()
	server.answer(http.StatusInternalServerError)
	note := signedEvent(t, testOwnerSecret, 1, now, nil, "```mermaid\nA\n```")
	if err := publishAs(t, tenant, note); err != nil {
		t.Fatal(err)
	}
	queued := pendingViewIntents(t, tenant, "diagrams", note.ID)
	if len(queued) != 1 {
		t.Fatalf("queued = %d", len(queued))
	}
	runViewIntent(t, tenant, queued[0])
	queued = pendingViewIntents(t, tenant, "diagrams", note.ID)
	if len(queued) != 1 || queued[0].EventID != note.ID+"#2" || queued[0].NextAt.Unix() < now+55 || queued[0].NextAt.Unix() > now+70 {
		t.Fatalf("second attempt = %+v", queued)
	}
	runViewIntent(t, tenant, queued[0])
	queued = pendingViewIntents(t, tenant, "diagrams", note.ID)
	if len(queued) != 1 || queued[0].EventID != note.ID+"#3" || queued[0].NextAt.Unix() < now+295 || queued[0].NextAt.Unix() > now+310 {
		t.Fatalf("third attempt = %+v", queued)
	}
	runViewIntent(t, tenant, queued[0])
	if queued = pendingViewIntents(t, tenant, "diagrams", note.ID); len(queued) != 0 {
		t.Fatalf("fourth attempt queued: %+v", queued)
	}
	var failures, enabled int
	var status string
	state := func() {
		t.Helper()
		if err := tenant.store.DB().QueryRowContext(ctx, "SELECT failures,enabled,last_status FROM custom_views WHERE name='diagrams'").Scan(&failures, &enabled, &status); err != nil {
			t.Fatal(err)
		}
	}
	state()
	if failures != 3 || enabled != 1 || status != "HTTP 500" {
		t.Fatalf("after three failures: %d %d %q", failures, enabled, status)
	}
	if server.count() != 3 {
		t.Fatalf("requests = %d", server.count())
	}
	// A 200 that is not JSON is a failure too.
	server.mu.Lock()
	server.status, server.body = http.StatusOK, "<html>"
	server.mu.Unlock()
	other := signedEvent(t, testOwnerSecret, 1, now+1, nil, "```mermaid\nB\n```")
	if err := publishAs(t, tenant, other); err != nil {
		t.Fatal(err)
	}
	runPendingView(t, tenant, "diagrams", other.ID)
	state()
	if failures != 4 || status != "invalid response" {
		t.Fatalf("after bad json: %d %q", failures, status)
	}
	// The twentieth failure in a row pauses the view; nothing new is queued.
	if _, err := tenant.store.DB().ExecContext(ctx, "UPDATE custom_views SET failures=19 WHERE name='diagrams'"); err != nil {
		t.Fatal(err)
	}
	server.answer(http.StatusBadGateway)
	third := signedEvent(t, testOwnerSecret, 1, now+2, nil, "```mermaid\nC\n```")
	if err := publishAs(t, tenant, third); err != nil {
		t.Fatal(err)
	}
	runPendingView(t, tenant, "diagrams", third.ID)
	state()
	if failures != 20 || enabled != 0 || status != "paused after 20 failures: HTTP 502" {
		t.Fatalf("after twenty failures: %d %d %q", failures, enabled, status)
	}
	if queued := pendingViewIntents(t, tenant, "diagrams", third.ID); len(queued) != 0 {
		t.Fatalf("paused view queued a retry: %+v", queued)
	}
	fourth := signedEvent(t, testOwnerSecret, 1, now+3, nil, "```mermaid\nD\n```")
	if err := publishAs(t, tenant, fourth); err != nil {
		t.Fatal(err)
	}
	if queued := pendingViewIntents(t, tenant, "diagrams", fourth.ID); len(queued) != 0 {
		t.Fatal("paused view queued a transform")
	}
	// Resuming clears the count; a success resets it after a failure.
	if _, err := tenant.Execute(ctx, tenant.Policy().Owner, "resumecustomview", []json.RawMessage{json.RawMessage(`"diagrams"`)}); err != nil {
		t.Fatal(err)
	}
	server.answer(http.StatusOK, svgArtifact(0))
	fifth := signedEvent(t, testOwnerSecret, 1, now+4, nil, "```mermaid\nE\n```")
	if err := publishAs(t, tenant, fifth); err != nil {
		t.Fatal(err)
	}
	runPendingView(t, tenant, "diagrams", fifth.ID)
	state()
	if failures != 0 || enabled != 1 || status != "ok" {
		t.Fatalf("after success: %d %d %q", failures, enabled, status)
	}
}

func TestCustomViewArtifactValidation(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte(testPNGRaw))
	for _, tc := range []struct {
		name     string
		artifact viewArtifact
		max      int
		want     string
	}{
		{"svg", viewArtifact{Type: "image/svg+xml", Body: testSVG}, 1024, ""},
		{"svg fragment href", viewArtifact{Type: "image/svg+xml", Body: `<svg><use href="#a"/><a xlink:href='#b'/></svg>`}, 1024, ""},
		{"png", viewArtifact{Type: "image/png", Body: png}, 1024, ""},
		{"png raw base64", viewArtifact{Type: "image/png", Body: strings.TrimRight(png, "=")}, 1024, ""},
		{"type", viewArtifact{Type: "text/html", Body: "<p>"}, 1024, "invalid: artifact type"},
		{"svg size", viewArtifact{Type: "image/svg+xml", Body: testSVG}, 10, "invalid: artifact exceeds"},
		{"png size", viewArtifact{Type: "image/png", Body: png}, 4, "invalid: artifact exceeds"},
		{"png base64", viewArtifact{Type: "image/png", Body: "not base64!"}, 1024, "invalid: png body must be base64"},
		{"png signature", viewArtifact{Type: "image/png", Body: base64.StdEncoding.EncodeToString([]byte("GIF89a"))}, 1024, "invalid: png body is not a PNG"},
		{"svg element", viewArtifact{Type: "image/svg+xml", Body: "<div/>"}, 1024, "invalid: svg body has no svg"},
		{"script", viewArtifact{Type: "image/svg+xml", Body: `<svg><SCRIPT>alert(1)</SCRIPT></svg>`}, 1024, "invalid: svg contains script"},
		{"handler", viewArtifact{Type: "image/svg+xml", Body: `<svg onload = "x()"/>`}, 1024, "invalid: svg contains an event handler"},
		{"foreign object", viewArtifact{Type: "image/svg+xml", Body: `<svg><foreignobject/></svg>`}, 1024, "invalid: svg contains a foreign object"},
		{"javascript href", viewArtifact{Type: "image/svg+xml", Body: `<svg><a href="javascript:alert(1)"/></svg>`}, 1024, "invalid: svg contains a javascript"},
		{"external href", viewArtifact{Type: "image/svg+xml", Body: `<svg><image href="https://evil.example/x.png"/></svg>`}, 1024, "invalid: svg references an external"},
		{"external xlink", viewArtifact{Type: "image/svg+xml", Body: `<svg><use xlink:href='/x.svg#a'/></svg>`}, 1024, "invalid: svg references an external"},
		{"unquoted href", viewArtifact{Type: "image/svg+xml", Body: `<svg><a href=data:text/html,x>`}, 1024, "invalid: svg references an external"},
	} {
		_, err := checkArtifact(tc.artifact, tc.max)
		if tc.want == "" && err != nil {
			t.Errorf("%s: unexpected %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.HasPrefix(err.Error(), tc.want)) {
			t.Errorf("%s: err = %v, want prefix %q", tc.name, err, tc.want)
		}
	}
	if body, err := checkArtifact(viewArtifact{Type: "image/png", Body: " " + png + "\n"}, 1024); err != nil || body != png {
		t.Fatalf("png normalization: %q %v", body, err)
	}
}

func TestCustomViewMembersAudienceStaysOffTheStore(t *testing.T) {
	tenant, server, added := viewTenant(t, []int{1}, map[string]any{"audience": "members"})
	ctx := context.Background()
	member, _ := event.PublicKey(testMemberSecret)
	setRole(t, tenant, member, "member")
	now := time.Now().Unix()
	server.answer(http.StatusOK, svgArtifact(0))
	note := signedEvent(t, testOwnerSecret, 1, now, nil, "```mermaid\nA\n```")
	if err := publishAs(t, tenant, note); err != nil {
		t.Fatal(err)
	}
	runPendingView(t, tenant, "diagrams", note.ID)
	hash := views.Hash("mermaid", "A")
	if _, ok := storedArtifact(t, tenant, "diagrams", hash); ok {
		t.Fatal("a members-only artifact became a stored record")
	}
	var raw string
	if err := tenant.store.DB().QueryRowContext(ctx, "SELECT raw FROM custom_view_artifacts WHERE view='diagrams' AND hash=?", hash).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	signed, err := event.Parse([]byte(raw))
	if err != nil || signed.PubKey != tenant.records.PublicKey() || event.Tag(signed, "d") != "bind.ws/view/diagrams/"+hash {
		t.Fatalf("members artifact record = %+v %v", signed, err)
	}
	path := "/views/diagrams/" + hash + ".svg"
	if res := getArtifact(t, tenant, path, ""); res.Code != http.StatusUnauthorized {
		t.Fatalf("guest: %d", res.Code)
	}
	if res := getArtifact(t, tenant, path, strings.Repeat("7", 64)); res.Code != http.StatusForbidden {
		t.Fatalf("outsider: %d", res.Code)
	}
	for _, secret := range []string{testMemberSecret, testOwnerSecret} {
		res := getArtifact(t, tenant, path, secret)
		if res.Code != http.StatusOK || res.Body.String() != testSVG || res.Header().Get("Cache-Control") != "private, no-cache" || res.Header().Get("ETag") == "" {
			t.Fatalf("member: %d %v", res.Code, res.Header())
		}
	}
	// A public view on a members-only relay follows the read rule.
	p := tenant.Policy()
	p.Reads = "members"
	if err := tenant.applyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	addView(t, tenant, map[string]any{"name": "open", "kinds": []int{1}, "transform": added["transform"], "languages": []string{"dot"}})
	other := signedEvent(t, testOwnerSecret, 1, now+1, nil, "```dot\nB\n```")
	if err := publishAs(t, tenant, other); err != nil {
		t.Fatal(err)
	}
	runPendingView(t, tenant, "open", other.ID)
	openPath := "/views/open/" + views.Hash("dot", "B") + ".svg"
	if res := getArtifact(t, tenant, openPath, ""); res.Code != http.StatusUnauthorized && res.Code != http.StatusForbidden {
		t.Fatalf("guest on a members relay: %d", res.Code)
	}
	if res := getArtifact(t, tenant, openPath, testMemberSecret); res.Code != http.StatusOK {
		t.Fatalf("member on a members relay: %d %s", res.Code, res.Body.String())
	}
}

func TestCustomViewHourlyTickQueuesRecentSources(t *testing.T) {
	tenant, server, _ := viewTenant(t, []int{1}, map[string]any{"trigger": "hourly"})
	ctx := context.Background()
	now := time.Now().Unix()
	server.answer(http.StatusOK, svgArtifact(0))
	note := signedEvent(t, testOwnerSecret, 1, now, nil, "```mermaid\nA\n```")
	if err := publishAs(t, tenant, note); err != nil {
		t.Fatal(err)
	}
	if queued := pendingViewIntents(t, tenant, "diagrams", note.ID); len(queued) != 0 {
		t.Fatal("an hourly view queued at write time")
	}
	if err := tenant.tickCustomViews(ctx, now); err != nil {
		t.Fatal(err)
	}
	queued := pendingViewIntents(t, tenant, "diagrams", note.ID)
	if len(queued) != 1 || !strings.HasPrefix(queued[0].EventID, note.ID+"@") {
		t.Fatalf("hourly queue = %+v", queued)
	}
	runViewIntent(t, tenant, queued[0])
	if _, ok := storedArtifact(t, tenant, "diagrams", views.Hash("mermaid", "A")); !ok {
		t.Fatal("hourly artifact missing")
	}
	// Not due again for an hour; a backfill through runcustomview queues now.
	if err := tenant.tickCustomViews(ctx, now+60); err != nil {
		t.Fatal(err)
	}
	if queued := pendingViewIntents(t, tenant, "diagrams", note.ID); len(queued) != 0 {
		t.Fatal("hourly view queued again within the hour")
	}
	ran, err := tenant.Execute(ctx, tenant.Policy().Owner, "runcustomview", []json.RawMessage{json.RawMessage(`"diagrams"`)})
	if err != nil || ran.(map[string]any)["queued"] != 1 {
		t.Fatalf("runcustomview: %v %v", ran, err)
	}
	queued = pendingViewIntents(t, tenant, "diagrams", note.ID)
	if len(queued) != 1 {
		t.Fatalf("backfill queue = %+v", queued)
	}
	runViewIntent(t, tenant, queued[0])
	if server.count() != 1 {
		t.Fatalf("backfill re-rendered a block with an artifact: %d requests", server.count())
	}
}

func TestCustomViewRendersTheReadmeAtTheStateHead(t *testing.T) {
	tenant, server, _ := viewTenant(t, []int{event.KIND_REPO_STATE}, nil)
	ctx := context.Background()
	p := tenant.Policy()
	p.Features.Grasp = true
	if err := tenant.applyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	owner := tenant.Policy().Owner
	announcement := signedEvent(t, testOwnerSecret, event.KIND_REPO, time.Now().Unix(), [][]string{{"d", "docs"}, {"clone", "http://127.0.0.1:8080/npub10xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqpkge6d/docs.git"}, {"relays", "ws://127.0.0.1:8080"}}, "")
	if err := publishAs(t, tenant, announcement); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.git")
	gitTest(t, "init", "--bare", source)
	work := filepath.Join(t.TempDir(), "work")
	gitTest(t, "clone", source, work)
	gitTest(t, "-C", work, "config", "user.email", "test@example.com")
	gitTest(t, "-C", work, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# Docs\n\n```go\nx\n```\n\n```mermaid\ngraph LR; a-->b;\n```\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, "-C", work, "add", "README.md")
	gitTest(t, "-C", work, "commit", "-m", "readme")
	gitTest(t, "-C", work, "branch", "-M", "main")
	sha := strings.TrimSpace(gitOutput(t, "-C", work, "rev-parse", "HEAD"))
	state := signedEvent(t, testOwnerSecret, event.KIND_REPO_STATE, time.Now().Unix(), [][]string{{"d", "docs"}, {"HEAD", "ref: refs/heads/main"}, {"refs/heads/main", sha}}, "")
	if err := publishAs(t, tenant, state); err != nil {
		t.Fatal(err)
	}
	if queued := pendingViewIntents(t, tenant, "diagrams", state.ID); len(queued) != 0 {
		t.Fatal("a state was queued before its objects arrived")
	}
	relay := httptest.NewServer(tenant)
	defer relay.Close()
	gitTest(t, "-C", work, "push", relay.URL+"/npub10xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqpkge6d/docs.git", "refs/heads/main:refs/heads/main")
	blocks, err := tenant.sourceBlocks(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 || blocks[1].Index != 1 || blocks[1].Lang != "mermaid" || blocks[1].Source != "graph LR; a-->b;" {
		t.Fatalf("readme blocks = %+v", blocks)
	}
	queued := pendingViewIntents(t, tenant, "diagrams", state.ID)
	if len(queued) != 1 {
		t.Fatalf("state intents after push = %d", len(queued))
	}
	server.answer(http.StatusOK, svgArtifact(1))
	runViewIntent(t, tenant, queued[0])
	var request map[string]any
	if err := json.Unmarshal(server.requests[0], &request); err != nil {
		t.Fatal(err)
	}
	coordinate := "30618:" + owner + ":docs"
	if source := request["source"].(map[string]any); source["id"] != coordinate || source["kind"] != float64(30618) {
		t.Fatalf("state source = %v", source)
	}
	if blocks := request["blocks"].([]any); len(blocks) != 1 || blocks[0].(map[string]any)["index"] != float64(1) {
		t.Fatalf("state blocks = %v", blocks)
	}
	artifact, ok := storedArtifact(t, tenant, "diagrams", views.Hash("mermaid", "graph LR; a-->b;"))
	if !ok || event.Tag(artifact, "a") != coordinate || event.Tag(artifact, "e") != "" || event.Tag(artifact, "block") != "1" {
		t.Fatalf("state artifact = %+v %v", artifact, ok)
	}
}

func TestMCPCustomViewTools(t *testing.T) {
	app, tenant := testTenant(t)
	member, _ := event.PublicKey(testMemberSecret)
	setRole(t, tenant, member, "member")
	call := func(name string, arguments map[string]any, secret string) (map[string]any, bool) {
		t.Helper()
		w, response := mcpCall{method: "tools/call", name: name, arguments: arguments, sign: true, secret: secret}.do(t, app, "/mcp")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		return mcpToolResult(t, response)
	}
	structured := func(result map[string]any) any {
		value, _ := result["structuredContent"].(map[string]any)
		return value["result"]
	}
	text := func(result map[string]any) string {
		content, _ := result["content"].([]any)
		if len(content) == 0 {
			return ""
		}
		return content[0].(map[string]any)["text"].(string)
	}
	_, response := mcpCall{method: "tools/list", sign: true}.do(t, app, "/mcp")
	names := map[string]bool{}
	for _, tool := range response.Result.(map[string]any)["tools"].([]any) {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"list_custom_views", "add_custom_view", "remove_custom_view", "pause_custom_view", "resume_custom_view", "run_custom_view"} {
		if !names[want] {
			t.Fatalf("missing tool %s", want)
		}
	}
	definition := map[string]any{"name": "diagrams", "kinds": []any{1.0, 30023.0}, "transform": "https://render.example/tiny", "languages": []any{"mermaid"}}
	result, isError := call("add_custom_view", definition, testMemberSecret)
	if !isError || !strings.Contains(text(result), "restricted") {
		t.Fatalf("add_custom_view as member: %v %s", isError, text(result))
	}
	result, isError = call("add_custom_view", definition, "")
	added, _ := structured(result).(map[string]any)
	secret, _ := added["secret"].(string)
	if isError || len(secret) != 64 || added["name"] != "diagrams" || added["trigger"] != "write" {
		t.Fatalf("add_custom_view: %v %s", isError, text(result))
	}
	definition["name"], definition["secret"], definition["trigger"], definition["audience"], definition["max_bytes"] = "charts", "shared-secret-value-1234", "hourly", "members", 4096.0
	result, isError = call("add_custom_view", definition, "")
	if pasted, _ := structured(result).(map[string]any); isError || pasted["secret"] != "shared-secret-value-1234" || pasted["trigger"] != "hourly" || pasted["audience"] != "members" || pasted["maxBytes"] != float64(4096) {
		t.Fatalf("add_custom_view with a secret: %v %s", isError, text(result))
	}
	result, isError = call("list_custom_views", nil, "")
	rows, _ := structured(result).([]any)
	if isError || len(rows) != 2 || strings.Contains(text(result), secret) || strings.Contains(text(result), "shared-secret-value-1234") {
		t.Fatalf("list_custom_views: %v %s", isError, text(result))
	}
	if result, isError = call("list_custom_views", nil, testMemberSecret); !isError {
		t.Fatalf("list_custom_views as member: %s", text(result))
	}
	result, isError = call("pause_custom_view", map[string]any{"name": "diagrams"}, "")
	if paused, _ := structured(result).(map[string]any); isError || paused["enabled"] != false {
		t.Fatalf("pause_custom_view: %v %s", isError, text(result))
	}
	result, isError = call("resume_custom_view", map[string]any{"name": "diagrams"}, "")
	if resumed, _ := structured(result).(map[string]any); isError || resumed["enabled"] != true {
		t.Fatalf("resume_custom_view: %v %s", isError, text(result))
	}
	result, isError = call("run_custom_view", map[string]any{"name": "diagrams"}, "")
	if ran, _ := structured(result).(map[string]any); isError || ran["queued"] != float64(0) {
		t.Fatalf("run_custom_view: %v %s", isError, text(result))
	}
	result, isError = call("remove_custom_view", map[string]any{"name": "charts"}, "")
	if removed, _ := structured(result).(map[string]any); isError || removed["removed"] != true {
		t.Fatalf("remove_custom_view: %v %s", isError, text(result))
	}
	result, isError = call("remove_custom_view", map[string]any{"name": "charts"}, "")
	if !isError || !strings.Contains(text(result), "not found") {
		t.Fatalf("remove_custom_view twice: %v %s", isError, text(result))
	}
}

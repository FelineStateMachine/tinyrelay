package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func (t *Tenant) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-SHA-256, X-Content-Length, X-Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		t.router.HandleHTTP(w, r)
		return
	}
	if t.sites != nil && t.sites.MatchesHost(r.Host) {
		t.sites.Handler().ServeHTTP(w, r)
		return
	}
	if t.git != nil && (strings.HasPrefix(r.URL.Path, "/npub1") || strings.HasPrefix(r.URL.Path, "/prs/")) {
		t.git.ServeHTTP(w, r)
		return
	}
	if t.blobs != nil && (r.URL.Path == "/upload" || r.URL.Path == "/mirror" || r.URL.Path == "/report" || r.URL.Path == "/nip96" || strings.HasPrefix(r.URL.Path, "/.well-known/nostr/nip96") || strings.HasPrefix(r.URL.Path, "/list/") || len(r.URL.Path) == 65 || len(r.URL.Path) == 69) {
		t.blobs.Handler().ServeHTTP(w, r)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/nostr+json") {
		t.information(w, r)
		return
	}
	if r.Method == http.MethodPost && strings.Contains(r.Header.Get("Content-Type"), "application/nostr+json+rpc") {
		t.manageHTTP(w, r)
		return
	}
	if r.Method == http.MethodPost && (r.URL.Path == "/events" || r.URL.Path == "/query" || r.URL.Path == "/count") {
		t.bridge(w, r)
		return
	}
	if r.URL.Path == "/.well-known/nostr.json" {
		t.nip05(w, r)
		return
	}
	if r.URL.Path == "/healthz" {
		writeJSON(w, 200, map[string]any{"status": "ready"})
		return
	}
	if t.ui != nil {
		t.ui.ServeHTTP(w, r)
		return
	}
	if r.URL.Path == "/" {
		t.home(w, r)
		return
	}
	http.NotFound(w, r)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "could not encode response", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(raw); err != nil {
		return
	}
}

func (t *Tenant) requestBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	var reader io.Reader = r.Body
	if t.app.cfg.MaxMessageBytes > 0 {
		reader = http.MaxBytesReader(w, r.Body, t.app.cfg.MaxMessageBytes)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (t *Tenant) session(r *http.Request, body []byte) (relay.Session, error) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	s := relay.Session{RelayURL: t.RelayURL(), RemoteIP: ip}
	url := t.publicURL + r.URL.RequestURI()
	e, err := t.auth.VerifyNIP98(r.Header.Get("Authorization"), url, r.Method, string(body))
	if err != nil {
		return s, err
	}
	s.PubKeys = []string{e.PubKey}
	return s, nil
}

func (t *Tenant) bridge(w http.ResponseWriter, r *http.Request) {
	body, err := t.requestBody(w, r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s, err := t.session(r, body)
	if err != nil {
		writeJSON(w, 401, map[string]string{"error": err.Error()})
		return
	}
	if r.URL.Path == "/events" {
		t.httpPublish(w, r, s, body)
		return
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || len(raw) == 0 {
		writeJSON(w, 400, map[string]string{"error": "invalid: body must be a non-empty array of filters"})
		return
	}
	filters := make([]event.Filter, len(raw))
	for i, b := range raw {
		f, err := event.ParseFilter(b)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		filters[i] = f
	}
	var result any
	if r.URL.Path == "/count" {
		result, err = t.Count(r.Context(), filters, s)
	} else {
		result, err = t.Query(r.Context(), filters, s)
	}
	if err != nil {
		writeJSON(w, statusFor(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, result)
}

func (t *Tenant) httpPublish(w http.ResponseWriter, r *http.Request, s relay.Session, body []byte) {
	e, err := event.Parse(body)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	message, err := t.router.Publish(r.Context(), e, s)
	status := 200
	if err != nil {
		message = err.Error()
		status = 400
	}
	writeJSON(w, status, map[string]any{"event_id": e.ID, "accepted": err == nil, "message": message})
}

func statusFor(err error) int {
	if strings.HasPrefix(err.Error(), "auth-required:") {
		return 401
	}
	if strings.HasPrefix(err.Error(), "restricted:") || strings.HasPrefix(err.Error(), "blocked:") {
		return 403
	}
	return 400
}

func (t *Tenant) information(w http.ResponseWriter, r *http.Request) {
	p := t.Policy()
	nips := []int{1, 9, 11, 13, 17, 40, 42, 62, 67, 70, 86, 98}
	if p.Features.Count {
		nips = append(nips, 45)
	}
	if p.Features.Search != "off" {
		nips = append(nips, 50)
	}
	if p.Features.Sync {
		nips = append(nips, 77)
	}
	if p.Features.Names {
		nips = append(nips, 5)
	}
	if p.Features.Signer {
		nips = append(nips, 46)
	}
	name := p.Name
	if name == "" {
		name = t.meta.Name
	}
	limits := map[string]any{"max_subid_length": 64, "auth_required": p.Reads != "open", "payment_required": false, "restricted_writes": p.Writes != "open"}
	if t.app.cfg.MaxMessageBytes > 0 {
		limits["max_message_length"] = t.app.cfg.MaxMessageBytes
	}
	if p.MaxFuture > 0 {
		limits["created_at_upper_limit"] = p.MaxFuture
	}
	if p.MinPow > 0 {
		limits["min_pow_difficulty"] = p.MinPow
	}
	info := map[string]any{"name": name, "description": p.Description, "pubkey": p.Owner, "supported_nips": nips, "software": "https://github.com/FelineStateMachine/tinyrelay", "version": t.app.cfg.Version, "self_url": t.RelayURL(), "limitation": limits}
	for key, value := range map[string]string{"icon": p.Icon, "banner": p.Banner, "contact": p.Contact, "posting_policy": p.PostingPolicy, "privacy_policy": p.PrivacyPolicy} {
		if value != "" {
			info[key] = value
		}
	}
	if p.JoinTerms != "" {
		info["terms_of_service"] = t.publicURL + "/terms"
	}
	if len(p.Tags) > 0 {
		info["tags"] = p.Tags
	}
	if len(p.LanguageTags) > 0 {
		info["language_tags"] = p.LanguageTags
	}
	if len(p.RelayCountries) > 0 {
		info["relay_countries"] = p.RelayCountries
	}
	raw, err := json.Marshal(info)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/nostr+json")
	if _, err := w.Write(raw); err != nil {
		return
	}
}

func (t *Tenant) nip05(w http.ResponseWriter, r *http.Request) {
	if !t.Policy().Features.Names {
		http.NotFound(w, r)
		return
	}
	result, err := t.community.NIP05(r.Context(), r.URL.Query().Get("name"), t.RelayURL())
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "name not found"})
		return
	}
	writeJSON(w, 200, result)
}

func (t *Tenant) home(w http.ResponseWriter, r *http.Request) {
	tmpl, err := template.New("home").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>{{.Name}}</title><h1>{{.Name}}</h1><p>{{.Description}}</p><p>Relay: <code>{{.URL}}</code></p><p><a href="./people">People</a> · <a href="./manage">Manage relay</a></p></html>`)
	if err != nil {
		http.Error(w, "page unavailable", 500)
		return
	}
	p := t.Policy()
	name := p.Name
	if name == "" {
		name = t.meta.Name
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, map[string]string{"Name": name, "Description": p.Description, "URL": t.RelayURL()}); err != nil {
		t.app.telemetry.Logger().Error("render page", "error", err)
	}
}

func (t *Tenant) manageHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := t.requestBody(w, r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	var call struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &call); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid: malformed management request"})
		return
	}
	actor := ""
	if call.Method != "supportedmethods" {
		s, err := t.session(r, body)
		if err != nil {
			writeJSON(w, 401, map[string]string{"error": err.Error()})
			return
		}
		actor = s.PubKeys[0]
	}
	result, err := t.Execute(r.Context(), actor, call.Method, call.Params)
	if err != nil {
		writeJSON(w, statusFor(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"result": result})
}

func (t *Tenant) Execute(ctx context.Context, actor, method string, params []json.RawMessage) (any, error) {
	if method == "supportedmethods" {
		methods := append([]string{"supportedmethods", "getpolicy", "setpolicy", "stats", "listlisthistory", "restorelist"}, t.community.Methods()...)
		if t.config != nil {
			methods = append(methods, "exportconfig", "importconfig", "planconfig", "applypreset", "listpresets")
		}
		if t.replication != nil {
			methods = append(methods, t.replication.Methods()...)
		}
		return methods, nil
	}
	role, err := t.community.Role(ctx, actor)
	if err != nil {
		return nil, err
	}
	if method == "listlisthistory" {
		return t.store.ListHistory(ctx, actor, time.Now().Unix())
	}
	if method == "restorelist" {
		if len(params) == 0 {
			return nil, errors.New("invalid: event id required")
		}
		var id string
		if err := json.Unmarshal(params[0], &id); err != nil {
			return nil, err
		}
		return t.store.RestoreHistory(ctx, storage.RestoreOptions{Owner: actor, EventID: id, Now: time.Now().Unix()})
	}
	if role != "owner" && role != "moderator" {
		return nil, errors.New("restricted: owner or moderator required")
	}
	switch method {
	case "getpolicy":
		return t.Policy(), nil
	case "setpolicy":
		if role != "owner" {
			return nil, errors.New("restricted: only the owner sets policy")
		}
		if len(params) == 0 {
			return nil, errors.New("invalid: policy patch required")
		}
		var patch map[string]json.RawMessage
		if err := json.Unmarshal(params[0], &patch); err != nil {
			return nil, err
		}
		delete(patch, "owner")
		return t.setPolicy(ctx, patch)
	case "stats":
		return t.store.Stats(ctx)
	default:
		if t.config != nil && containsString([]string{"exportconfig", "importconfig", "planconfig", "applypreset", "listpresets"}, method) {
			return t.config.Execute(ctx, method, params)
		}
		if t.replication != nil && containsString(t.replication.Methods(), method) {
			values := make([]any, len(params))
			for i, raw := range params {
				var value any
				if err := json.Unmarshal(raw, &value); err != nil {
					return nil, err
				}
				values[i] = value
			}
			return t.replication.Execute(ctx, method, values...)
		}
		return t.community.Execute(ctx, actor, method, params)
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

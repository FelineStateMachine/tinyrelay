package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/blob"
	"github.com/FelineStateMachine/tinyrelay/internal/domains"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func (t *Tenant) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, MCP-Protocol-Version, Mcp-Method, Mcp-Name, X-SHA-256, X-Content-Length, X-Content-Type, Upload-Type, Upload-Length, Upload-Offset")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Expose-Headers", "Allow, X-Reason")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodOptions {
		if t.blobs != nil && t.Policy().Features.Files && blob.HandlesPath(r.URL.Path) {
			t.blobs.Handler().ServeHTTP(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		t.healthHTTP(w, r)
		return
	}
	if t.unclaimedSiteHost(r.Host) {
		// A host under the site domain that no site claims must not fall
		// through to the relay: signed requests would otherwise verify
		// against a URL the relay does not own.
		http.NotFound(w, r)
		return
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		t.router.HandleHTTP(w, r)
		return
	}
	if isRoomStreamPath(r.URL.Path) {
		t.roomStreamHTTP(w, r)
		return
	}
	if r.URL.Path != "/backups/restore" && r.URL.Path != "/manage/jobs/status" {
		ctx, done, err := t.beginOperation(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer done()
		r = r.WithContext(ctx)
	}
	if t.tryDataHTTP(w, r) {
		return
	}
	if r.URL.Path == "/session" || r.URL.Path == "/session/logout" {
		t.sessionHTTP(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/push/") || r.URL.Path == "/inbox/seen" {
		t.pushHTTP(w, r)
		return
	}
	if r.URL.Path == "/mcp" {
		t.mcpHTTP(w, r)
		return
	}
	if r.URL.Path == "/llms.txt" {
		t.llmsHTTP(w, r)
		return
	}
	if t.tryBrowseHTTP(w, r) {
		return
	}
	if isViewArtifactPath(r.URL.Path) {
		t.viewArtifactHTTP(w, r)
		return
	}
	if t.sites != nil && t.sites.MatchesHost(r.Host) {
		t.sites.Handler().ServeHTTP(w, r)
		return
	}
	if t.git != nil && (strings.HasPrefix(r.URL.Path, "/npub1") || strings.HasPrefix(r.URL.Path, "/npub/") || strings.HasPrefix(r.URL.Path, "/prs/")) {
		p := t.Policy()
		if !p.Features.Grasp || (strings.HasPrefix(r.URL.Path, "/prs/") && !p.Features.Grasp06) {
			http.NotFound(w, r)
			return
		}
		if p.Reads != "open" && !privatePolicy(p) {
			http.Error(w, "restricted: public Git hosting requires open reads", http.StatusForbidden)
			return
		}
		if privatePolicy(p) && strings.TrimSpace(r.Header.Get("Authorization")) == "" {
			writeGRASP08Challenge(w)
			return
		}
		if privatePolicy(p) {
			proof, authErr := t.auth.VerifyGRASP08(r.Header.Get("Authorization"), t.requestURL(r))
			if authErr != nil {
				writeGRASP08Challenge(w)
				return
			}
			if accessErr := t.requirePrivateAccess(r.Context(), proof.PubKey); accessErr != nil {
				writeGRASP08Challenge(w)
				return
			}
			r = privateGitRequest(r, proof)
		}
		t.git.ServeHTTP(w, r)
		return
	}
	if t.blobs != nil && blob.HandlesPath(r.URL.Path) {
		if !t.Policy().Features.Files {
			http.NotFound(w, r)
			return
		}
		t.blobs.Handler().ServeHTTP(w, r)
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
		if _, exists := r.URL.Query()["path"]; exists {
			t.webAddress(w, r)
			return
		}
		t.nip05(w, r)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/nostr+json") {
		t.information(w, r)
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

func writeGRASP08Challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Nostr method="GET"`)
	w.WriteHeader(http.StatusUnauthorized)
}

func (t *Tenant) webAddress(w http.ResponseWriter, r *http.Request) {
	actor, err := t.resolveUIActor(r)
	if err != nil {
		http.Error(w, err.Error(), 401)
		return
	}
	keys := []string{}
	if actor != "" {
		keys = append(keys, actor)
	}
	session := relay.Session{PubKeys: keys, RelayURL: t.RelayURL()}
	domains.WebAddressHandler(domains.AddressConfig{
		RelayURL: t.RelayURL(),
		Authorized: func(req *http.Request) bool {
			// resolveUIActor already verified and consumed the NIP-98 proof.
			// Revalidating it here would reject every signed request as replay.
			return req.Header.Get("Authorization") != "" && actor != ""
		},
		ReadAllowed: func(r *http.Request) bool {
			_, err := t.gate.Read(r.Context(), []event.Filter{{}}, session)
			return err == nil
		},
		Lookup: func(ctx context.Context, path string) (map[string]any, bool, error) {
			filter, ok := AddressFilterForPath(path, t.Policy().Owner, t.records.PublicKey())
			if !ok {
				return nil, false, nil
			}
			raw, err := json.Marshal(filter)
			if err != nil {
				return nil, false, err
			}
			f, err := event.ParseFilter(raw)
			if err != nil {
				return nil, false, err
			}
			rows, err := t.Query(ctx, []event.Filter{f}, session)
			return filter, len(rows) > 0, err
		},
	}).ServeHTTP(w, r)
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
	url := t.requestURL(r)
	e, err := t.auth.VerifyNIP98(r.Header.Get("Authorization"), url, r.Method, string(body))
	if err != nil {
		return s, err
	}
	s.PubKeys = []string{e.PubKey}
	return s, nil
}

// cookieReadSession lets a signed-in browser read through the query bridge
// without a signature, so pages do not prompt the signer just to show data.
// The Origin check keeps another site from reading with a visitor's cookie;
// publishing still needs a signed event.
func (t *Tenant) cookieReadSession(r *http.Request, s relay.Session) (relay.Session, error) {
	base, err := url.Parse(t.requestURL(r))
	if err != nil || r.Header.Get("Origin") != base.Scheme+"://"+base.Host {
		return s, errors.New("auth-required: sign this request")
	}
	actor, err := t.cookieActor(r)
	if err != nil {
		return s, err
	}
	if actor == "" {
		return s, errors.New("auth-required: sign this request")
	}
	s.PubKeys = []string{actor}
	return s, nil
}

func (t *Tenant) bridge(w http.ResponseWriter, r *http.Request) {
	body, err := t.requestBody(w, r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s, err := t.session(r, body)
	if err != nil && r.URL.Path != "/events" && r.Header.Get("Authorization") == "" {
		s, err = t.cookieReadSession(r, s)
	}
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
	nips := []int{}
	var agents *Capability
	for _, capability := range t.Capabilities(nil) {
		if capability.Status != "enabled" {
			continue
		}
		if capability.ID == "agents" {
			entry := capability
			agents = &entry
		}
		if number, ok := capabilityNIPNumber(capability.ID); ok {
			nips = append(nips, number)
		}
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
	if privatePolicy(p) {
		// GRASP-08 discovery is intentionally sparse. Clients need the service
		// identity and protocol markers; tenant presentation metadata stays
		// behind authenticated reads.
		info = t.privateNIP11()
		info["supported_nips"] = nips
		info["software"] = "https://github.com/FelineStateMachine/tinyrelay"
		info["version"] = t.app.cfg.Version
		info["self_url"] = t.RelayURL()
		info["limitation"] = limits
	}
	if t.records != nil {
		info["self"] = t.records.PublicKey()
	}
	if p.Features.Grasp && t.git != nil {
		info["supported_grasps"] = t.git.SupportedGRASPs()
	}
	if agents != nil {
		info["agents"] = *agents
	}
	if p.Features.Sites.Enabled && !privatePolicy(p) {
		base, _ := url.Parse(t.publicURL)
		domain := t.siteDomain()
		if base.Port() != "" {
			domain += ":" + base.Port()
		}
		info["nsites"] = map[string]any{"host": t.siteDomain(), "kinds": []int{15128, 35128, 5128}, "root": base.Scheme + "://<npub>." + domain, "named": base.Scheme + "://<pubkeyB36><dTag>." + domain, "snapshot": base.Scheme + "://v<snapshotIdB36>." + domain}
	}
	for key, value := range map[string]string{"icon": p.Icon, "banner": p.Banner, "contact": p.Contact, "posting_policy": p.PostingPolicy, "privacy_policy": p.PrivacyPolicy} {
		if privatePolicy(p) {
			break
		}
		if value != "" {
			info[key] = value
		}
	}
	if p.JoinTerms != "" && !privatePolicy(p) {
		info["terms_of_service"] = t.publicURL + "/terms"
	}
	if len(p.Tags) > 0 && !privatePolicy(p) {
		info["tags"] = p.Tags
	}
	if len(p.LanguageTags) > 0 && !privatePolicy(p) {
		info["language_tags"] = p.LanguageTags
	}
	if len(p.RelayCountries) > 0 && !privatePolicy(p) {
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
	if privatePolicy(t.Policy()) {
		actor, err := t.resolveUIActor(r)
		if err != nil || actor == "" {
			http.Error(w, "auth-required: private service metadata requires NIP-98", http.StatusUnauthorized)
			return
		}
		if err := t.requirePrivateAccess(r.Context(), actor); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	}
	result, err := t.community.NIP05(r.Context(), r.URL.Query().Get("name"), t.RelayURL())
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "name not found"})
		return
	}
	writeJSON(w, 200, result)
}

func (t *Tenant) home(w http.ResponseWriter, r *http.Request) {
	tmpl, err := template.New("home").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>{{.Name}}</title><h1>{{.Name}}</h1><p>{{.Description}}</p><p>Relay: <code>{{.URL}}</code></p><p><a href="./people">People</a> | <a href="./manage">Manage relay</a></p></html>`)
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
	ctx, done, admissionErr := t.beginOperation(ctx)
	if admissionErr != nil {
		return nil, admissionErr
	}
	defer done()
	if method == "supportedmethods" {
		methods := append([]string{"supportedmethods", "getpolicy", "setpolicy", "stats", "listlisthistory", "restorelist"}, t.community.Methods()...)
		methods = append(methods, "successionstatus", "setsuccession", "clearsuccession", "listpins", "pinevent", "unpinevent", "listviews", "notifytest", "notifystatus")
		if t.config != nil {
			methods = append(methods, "exportconfig", "importconfig", "planconfig", "applypreset", "listpresets")
		}
		if t.replication != nil {
			methods = append(methods, t.replication.Methods()...)
		}
		methods = append(methods, ManagementMethods()...)
		seen := make(map[string]bool)
		result := []string{}
		for _, method := range methods {
			if !seen[method] {
				seen[method] = true
				result = append(result, method)
			}
		}
		return result, nil
	}
	role, err := t.community.Role(ctx, actor)
	if err != nil {
		return nil, err
	}
	if result, handled, err := t.executeManagement(ctx, actor, method, params); handled {
		return result, err
	}
	if containsString(t.community.Methods(), method) {
		return t.executeCommunity(ctx, actor, method, params)
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
	if t.records != nil && containsString([]string{"successionstatus", "setsuccession", "clearsuccession", "listpins", "pinevent", "unpinevent", "listviews", "notifytest", "notifystatus"}, method) {
		return t.records.Execute(ctx, actor, method, params)
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
			if role != "owner" {
				return nil, errors.New("restricted: only the owner manages configuration")
			}
			return t.config.Execute(ctx, method, params)
		}
		if t.replication != nil && containsString(t.replication.Methods(), method) {
			if role != "owner" {
				return nil, errors.New("restricted: only the owner manages replication")
			}
			return t.replication.ExecuteRaw(ctx, method, params)
		}
		return t.executeCommunity(ctx, actor, method, params)
	}
}

func (t *Tenant) executeCommunity(ctx context.Context, actor, method string, params []json.RawMessage) (any, error) {
	if !communityACLMutation(method) {
		return t.community.Execute(ctx, actor, method, params)
	}
	result, err := t.router.ApplyACLChange(ctx, func(changeCtx context.Context) (any, error) {
		return t.community.Execute(changeCtx, actor, method, params)
	}, "blocked: relay access policy changed; subscribe again")
	return result, err
}

func communityACLMutation(method string) bool {
	switch method {
	case "setmember", "allowpubkey", "removemember", "unrulepubkey", "removesubtree", "approvejoin", "banpubkey", "banevent", "allowevent", "blockip", "unblockip", "resolvereport", "allowkind", "disallowkind", "unrulekind", "setblockedwords":
		return true
	default:
		return false
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

// unclaimedSiteHost reports whether host is a subdomain of the site domain
// that neither a hosted site nor a custom host claims.
func (t *Tenant) unclaimedSiteHost(host string) bool {
	host = strings.ToLower(strings.Split(host, ":")[0])
	domain := strings.ToLower(t.siteDomain())
	if domain == "" || host == domain || !strings.HasSuffix(host, "."+domain) {
		return false
	}
	if t.sites != nil && t.sites.MatchesHost(host) {
		return false
	}
	for _, custom := range t.Policy().CustomHosts {
		if strings.EqualFold(custom.Host, host) {
			return false
		}
	}
	return true
}

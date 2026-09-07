package daemon

import (
	"context"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/auth"
	"github.com/FelineStateMachine/tinyrelay/internal/catalog"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/templates"
)

// ServeLanding handles only the process landing page and permanent relay
// creation. It returns false for tenant routes so App.ServeHTTP can continue
// with normal host and /r/<name> resolution.
func (a *App) ServeLanding(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/" && r.URL.Path != "/relays" {
		return false
	}
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		a.landingPage(w, r)
		return true
	}
	if r.Method == http.MethodGet && r.URL.Path == "/relays" {
		a.relayList(w, r)
		return true
	}
	if r.Method == http.MethodPost && r.URL.Path == "/relays" {
		a.createRelay(w, r)
		return true
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return true
}

type landingRelay struct{ Name, URL, Status string }

func (a *App) landingPage(w http.ResponseWriter, r *http.Request) {
	relays, err := a.publicRelays(r.Context(), r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmpl := template.Must(template.New("landing").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>tiny relays</title><style>html,body{margin:0;padding:0}body{font:16px system-ui,sans-serif;line-height:1.4}.shell{width:360px;margin:1rem auto}.shell td{vertical-align:top}.content{padding:0 1rem}table{border-collapse:collapse;table-layout:fixed;width:100%}th,td{border:1px solid;padding:.35rem;text-align:left;overflow-wrap:anywhere}@media(min-width:979px){.shell{width:960px}}</style></head><body><table class="shell" role="presentation"><tr><td class="content"><h1>tiny relays</h1><p>Self-hosted relays with permanent ownership.</p><p><a href="/relays">Create a relay</a></p><h2>Public relays</h2>{{if .}}<table><thead><tr><th>Name</th><th>Status</th></tr></thead><tbody>{{range .}}<tr><td><a href="{{.URL}}">{{.Name}}</a></td><td>{{.Status}}</td></tr>{{end}}</tbody></table>{{else}}<p>No public relays are listed.</p>{{end}}</td></tr></table></body></html>`))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.Execute(w, relays)
}

func (a *App) relayList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tmpl := template.Must(template.New("create").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Create relay</title><style>html,body{margin:0;padding:0}body{font:16px system-ui,sans-serif;line-height:1.4}.shell{width:360px;margin:1rem auto}.shell td{vertical-align:top}.content{padding:0 1rem}table{border-collapse:collapse;table-layout:fixed;width:100%}th,td{border:1px solid;padding:.35rem;text-align:left;overflow-wrap:anywhere}input,select,button{font:inherit;max-width:100%;box-sizing:border-box}@media(min-width:979px){.shell{width:960px}}</style></head><body><table class="shell" role="presentation"><tr><td class="content"><h1>Create a permanent relay</h1><p>Use a NIP-07 browser signer. Ownership is assigned to the signer.</p><p>Browser provisioning is restricted to operator-approved owners. The command line remains available to the host operator.</p><form id="create" method="post" action="/relays"><table><tr><th>Name</th><td><input name="name" required pattern="[a-z0-9-]+"></td></tr><tr><th>Template</th><td><select name="template">{{range .Templates}}<option value="{{.}}">{{.}}</option>{{end}}</select></td></tr><tr><th>Source relay</th><td><input name="source" type="url" placeholder="wss://relay.example"></td></tr></table><input type="hidden" name="owner"><button>Create relay</button></form><p id="status"></p><script>
const form=document.getElementById('create'),status=document.getElementById('status');form.addEventListener('submit',async e=>{e.preventDefault();try{if(!window.nostr)throw Error('No NIP-07 signer found');const owner=await window.nostr.getPublicKey();const body=JSON.stringify({name:form.name.value,template:form.template.value,owner,source:form.source.value});const digest=await crypto.subtle.digest('SHA-256',new TextEncoder().encode(body));const hash=[...new Uint8Array(digest)].map(x=>x.toString(16).padStart(2,'0')).join('');const signed=await window.nostr.signEvent({kind:27235,created_at:Math.floor(Date.now()/1000),tags:[['u',location.origin+'/relays'],['method','POST'],['payload',hash]],content:''});const token=btoa(unescape(encodeURIComponent(JSON.stringify(signed))));const response=await fetch('/relays',{method:'POST',headers:{'Content-Type':'application/json','Authorization':'Nostr '+token},body});if(!response.ok)throw Error(await response.text());location.href=(await response.json()).url}catch(err){status.textContent=err.message}});</script><p><a href="/">Back</a></p></td></tr></table></body></html>`))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.Execute(w, struct{ Templates []string }{Templates: templates.Names()})
}

func (a *App) createRelay(w http.ResponseWriter, r *http.Request) {
	reader := io.Reader(r.Body)
	if a.cfg.MaxMessageBytes > 0 {
		reader = http.MaxBytesReader(w, r.Body, a.cfg.MaxMessageBytes)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var input struct {
		Name     string `json:"name"`
		Template string `json:"template"`
		Owner    string `json:"owner"`
		Source   string `json:"source"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		_ = r.ParseForm()
		input.Name, input.Template, input.Owner, input.Source = r.FormValue("name"), r.FormValue("template"), r.FormValue("owner"), r.FormValue("source")
	}
	if input.Template == "" {
		input.Template = "default"
	}
	if input.Source != "" {
		u, parseErr := url.Parse(input.Source)
		if parseErr != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "ws" && u.Scheme != "wss") {
			http.Error(w, "source must be an http, https, ws, or wss relay", 400)
			return
		}
	}
	requestedURL := a.publicURL(r, "") + "/relays"
	validator := auth.NewValidator(time.Now)
	proof, err := validator.VerifyNIP98(r.Header.Get("Authorization"), requestedURL, r.Method, string(body))
	if err != nil {
		http.Error(w, err.Error(), 401)
		return
	}
	if proof.PubKey != input.Owner {
		http.Error(w, "owner must match the signer", 403)
		return
	}
	if !a.provisionAllowed(proof.PubKey) {
		http.Error(w, "browser provisioning is disabled for this owner", http.StatusForbidden)
		return
	}
	consumed, err := a.catalog.ConsumeAuth(r.Context(), proof.ID, time.Now().Add(time.Minute))
	if err != nil {
		http.Error(w, "authorization storage failed", http.StatusInternalServerError)
		return
	}
	if !consumed {
		http.Error(w, "authorization has already been used", http.StatusUnauthorized)
		return
	}
	meta, err := a.Create(r.Context(), catalog.CreateOptions{Name: input.Name, Owner: input.Owner, Template: input.Template, Source: input.Source})
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": meta.Name, "owner": meta.Owner, "status": meta.Status, "url": strings.TrimSuffix(a.publicURL(r, ""), "/") + "/r/" + meta.Name})
}

func (a *App) provisionAllowed(pubkey string) bool {
	for _, allowed := range a.cfg.ProvisionOwners {
		if strings.EqualFold(strings.TrimSpace(allowed), pubkey) {
			return true
		}
	}
	return false
}

func (a *App) publicRelays(ctx context.Context, r *http.Request) ([]landingRelay, error) {
	items, err := a.catalog.List(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]landingRelay, 0, len(items))
	base := strings.TrimSuffix(a.publicURL(r, ""), "/")
	for _, item := range items {
		if item.Status != catalog.StatusReady {
			continue
		}
		store, openErr := storage.Open(ctx, item.Paths.Database)
		if openErr != nil {
			continue
		}
		var p policy.Policy
		policyErr := store.GetSetting(ctx, "policy", &p)
		_ = store.Close()
		if policyErr != nil || !p.DirectoryPublic {
			continue
		}
		result = append(result, landingRelay{Name: item.Name, URL: base + "/r/" + item.Name, Status: string(item.Status)})
	}
	return result, nil
}

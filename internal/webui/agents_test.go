package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

// agentsBackend answers listagents with three grants in every state and
// browseagent with the selected grant plus two events.
type agentsBackend struct {
	fakeBackend
	calls []string
}

var (
	agentActive  = strings.Repeat("1", 64)
	agentPaused  = strings.Repeat("2", 64)
	agentRevoked = strings.Repeat("3", 64)
	agentSite    = "npub1" + strings.Repeat("q", 58)
)

func (b *agentsBackend) Query(_ context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	b.calls = append(b.calls, method+":"+actor)
	b.params = params
	owner := b.policy.Owner
	now := time.Now().Unix()
	scope := func(rooms []string, repos []map[string]any, wiki string, kinds []int) map[string]any {
		return map[string]any{"rooms": rooms, "repos": repos, "wiki": wiki, "kinds": kinds, "rate": 60}
	}
	hermes := scope([]string{"build", "agents"}, []map[string]any{{"owner": owner, "identifier": "tinyrelay", "level": "maintain"}}, "propose", []int{9, 1111, 1621})
	hermes["sites"] = []map[string]any{{"label": agentSite, "ttl": 30, "encrypted": true}, {"label": "*"}}
	agents := []any{
		map[string]any{"pubkey": agentActive, "owner": owner, "name": "hermes", "expires": now + 86400*30, "paused": false, "revoked": 0, "lastEvent": now - 120, "scope": hermes},
		map[string]any{"pubkey": agentPaused, "owner": owner, "name": "reviewer-bot", "expires": now + 86400*30, "paused": true, "revoked": 0, "lastEvent": 0,
			"scope": scope([]string{"build"}, []map[string]any{{"owner": owner, "identifier": "tinyrelay", "level": "read"}}, "", nil)},
		map[string]any{"pubkey": agentRevoked, "owner": strings.Repeat("c", 64), "name": "", "expires": now + 86400*30, "paused": false, "revoked": now - 86400, "lastEvent": now - 86400*2,
			"scope": scope([]string{"general"}, nil, "", []int{9})},
	}
	switch method {
	case "listagents":
		return agents, nil
	case "listcallbacks":
		return []any{
			map[string]any{"id": "cb-active", "owner": agentActive, "host": "hooks.example", "filter": map[string]any{"kinds": []int{1621, 1111}, "#p": []string{agentActive}}, "created": now - 3600, "lastDelivery": now - 60, "lastStatus": "ok", "failures": 0, "paused": false},
			map[string]any{"id": "cb-paused", "owner": agentActive, "host": "worker.example:8443", "filter": map[string]any{"kinds": []int{9}}, "created": now - 3600, "lastDelivery": 0, "lastStatus": "paused after 20 failures: HTTP 500", "failures": 20, "paused": true},
			map[string]any{"id": "cb-other", "owner": strings.Repeat("c", 64), "host": "elsewhere.example", "filter": map[string]any{"kinds": []int{1}}, "created": now, "lastDelivery": 0, "lastStatus": "", "failures": 0, "paused": false},
		}, nil
	case "browseagent":
		var query struct {
			Agent string `json:"agent"`
		}
		_ = json.Unmarshal(params[0], &query)
		for _, agent := range agents {
			if agent.(map[string]any)["pubkey"] == query.Agent {
				return map[string]any{"agent": agent, "events": []any{
					map[string]any{"id": strings.Repeat("d", 64), "kind": 9, "pubkey": query.Agent, "created_at": now - 120, "content": "Draft is up as wiki: release-notes-1-4", "tags": [][]string{}},
					map[string]any{"id": strings.Repeat("e", 64), "kind": 1111, "pubkey": query.Agent, "created_at": now - 3600, "content": "Reviewed PR 23: two nits, see inline.", "tags": [][]string{}},
				}}, nil
			}
		}
		return nil, context.Canceled
	}
	return map[string]any{"method": method}, nil
}

func TestAgentsPageRendersTableCardsActivityAndGrantForm(t *testing.T) {
	backend := &agentsBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return backend.policy.Owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/manage/agents", nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, body)
	}
	if strings.Contains(body, "class=") {
		t.Fatal("agents page carries a class attribute")
	}
	if strings.Contains(body, "\u00b7") {
		t.Fatal("agents page contains a middle dot")
	}
	for _, want := range []string{
		`<a href="/manage/agents" aria-current="page">/agents</a>`,
		`<table id="rows">`,
		`<a href="#agent-` + agentActive + `">hermes</a>`,
		`rooms: build, agents | repos: tinyrelay (maintain) | wiki: propose | kinds: 9, 1111, 1621 | sites: ` + agentSite + `, *`,
		`<tr><th>sites</th><td><code>` + agentSite + `</code> 30 days encrypted<br><code>*</code></td></tr>`,
		`<tr><th>sites</th><td>none</td></tr>`,
		`<textarea name="sites" placeholder="npub1... ttl=30 encrypted&#10;* ttl=7"></textarea>`,
		`<td data-state="active">active | `,
		`<td data-state="paused">paused</td>`,
		`<td data-state="revoked">revoked | `,
		`<a href="#agent-` + agentRevoked + `">` + shortID(agentRevoked) + `</a>`,
		`<agent-card id="agent-` + agentActive + `">`,
		`<h3>hermes <small><span data-state="active">active</span> | signed by you | expires `,
		`<code>tinyrelay</code> maintain, owner <nostr-name pubkey="` + backend.policy.Owner + `"`,
		`<th>rate</th><td>60 events per minute</td>`,
		`<rpc-form method="pauseagent" refresh><input type="hidden" name="param" value="&quot;` + agentActive + `&quot;"><button>Pause</button></rpc-form>`,
		`<rpc-form method="revokeagent" refresh><input type="hidden" name="param" value="&quot;` + agentActive + `&quot;"><button>Revoke</button></rpc-form>`,
		`<rpc-form method="resumeagent" refresh><input type="hidden" name="param" value="&quot;` + agentPaused + `&quot;"><button>Resume</button></rpc-form>`,
		`<h3>Recent activity of hermes</h3>`,
		`<ol id="events">`,
		`<nostr-event id="event-` + strings.Repeat("d", 64) + `">`,
		`Reviewed PR 23: two nits, see inline.`,
		`<agent-grant>`,
		`<select name="key"><option value="generate">generate here, show once</option><option value="paste">paste a public key</option></select>`,
		`<textarea name="repos"`,
		`<input name="expires" type="date" value="` + dateAfter(90) + `" required>`,
		`<h4>Agents</h4><table><tr><th>active</th><td>1</td></tr><tr><th>paused</th><td>1</td></tr><tr><th>revoked</th><td>1</td></tr><tr><th>callbacks</th><td>2 active, 1 paused</td></tr></table>`,
		`<tr><th>callbacks</th><td><agent-callback data-state="active"><code>hooks.example</code> kinds 1621, 1111 | <span data-state="active" title="ok">active</span> | delivered <time datetime="`,
		`<rpc-form method="pausecallback" refresh><input type="hidden" name="param" value="&quot;cb-active&quot;"><button>Pause</button></rpc-form><rpc-form method="removecallback" refresh><input type="hidden" name="param" value="&quot;cb-active&quot;"><button>Remove</button></rpc-form></agent-callback>`,
		`<agent-callback data-state="paused"><code>worker.example:8443</code> kinds 9 | <span data-state="paused" title="paused after 20 failures: HTTP 500">paused</span> <rpc-form method="resumecallback" refresh><input type="hidden" name="param" value="&quot;cb-paused&quot;"><button>Resume</button></rpc-form>`,
		`<tr><th>callbacks</th><td>none</td></tr>`,
		`<code>/mcp</code> 2026-07-28`,
		`<rpc-form method="pauseallagents" refresh><button>Pause all agents</button></rpc-form>`,
		`<rpc-form method="resumeallagents" refresh><button>Resume all agents</button></rpc-form>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("agents page missing %q", want)
		}
	}
	// The revoked agent keeps its facts but offers no controls.
	revokedCard := body[strings.Index(body, `<agent-card id="agent-`+agentRevoked+`">`):]
	revokedCard = revokedCard[:strings.Index(revokedCard, "</agent-card>")]
	if strings.Contains(revokedCard, "<rpc-form") || !strings.Contains(revokedCard, `<span data-state="revoked">revoked</span>`) {
		t.Errorf("revoked card should show state without controls: %s", revokedCard)
	}
	if strings.Contains(body, "elsewhere.example") {
		t.Error("a callback of a key without a card was shown")
	}
	if got := strings.Join(backend.calls, " "); got != "listagents:"+backend.policy.Owner+" listcallbacks:"+backend.policy.Owner+" browseagent:"+backend.policy.Owner {
		t.Fatalf("backend calls = %s", got)
	}
	if !strings.Contains(string(backend.params[0]), agentActive) {
		t.Fatalf("default selection should be the first active agent: %s", backend.params[0])
	}
	// ?agent= selects whose activity shows.
	backend.calls = nil
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/manage/agents?agent="+agentPaused, nil))
	if body := recorder.Body.String(); !strings.Contains(body, `<h3>Recent activity of reviewer-bot</h3>`) {
		t.Fatalf("selected agent activity missing: %s", body[:min(500, len(body))])
	}
}

type emptyAgentsBackend struct{ fakeBackend }

func (b *emptyAgentsBackend) Query(ctx context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	_, _ = b.fakeBackend.Query(ctx, method, params, actor)
	return []any{}, nil
}

func TestAgentsPageWithoutAgentsAndForGuests(t *testing.T) {
	backend := &emptyAgentsBackend{fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	app, err := New(backend, Options{Actor: func(r *http.Request) (string, error) { return r.Header.Get("X-Actor"), nil }})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/manage/agents", nil)
	request.Header.Set("X-Actor", backend.policy.Owner)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "No agents yet.") || !strings.Contains(body, "<agent-grant>") || strings.Contains(body, "<agent-card") {
		t.Fatalf("empty agents page: %d %s", recorder.Code, body[:min(600, len(body))])
	}
	if backend.call != "listcallbacks:"+backend.policy.Owner {
		t.Fatalf("empty list should not query an agent: %s", backend.call)
	}

	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/manage/agents", nil))
	body = recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "Sign in to manage this relay.") || strings.Contains(body, "<agent-grant>") || strings.Contains(body, "pauseallagents") {
		t.Fatalf("guest saw the agents page: %d %s", recorder.Code, body[:min(600, len(body))])
	}
}

func TestAgentStateHelpers(t *testing.T) {
	now := time.Now().Unix()
	for _, tc := range []struct {
		agent map[string]any
		state string
		since string
	}{
		{map[string]any{"expires": float64(now + 3600), "lastEvent": float64(now - 60)}, "active", short(now - 60)},
		{map[string]any{"expires": float64(now + 3600), "paused": true}, "paused", ""},
		{map[string]any{"expires": float64(now + 3600), "revoked": float64(now - 10)}, "revoked", short(now - 10)},
		{map[string]any{"expires": float64(now - 10), "paused": true}, "revoked", "expired " + short(now-10)},
	} {
		if got := agentState(tc.agent); got != tc.state {
			t.Errorf("agentState(%v) = %q, want %q", tc.agent, got, tc.state)
		}
		if got := agentSince(tc.agent); got != tc.since {
			t.Errorf("agentSince(%v) = %q, want %q", tc.agent, got, tc.since)
		}
	}
	if got := agentScope(map[string]any{"scope": map[string]any{}}); got != "profile and relay list only" {
		t.Errorf("empty scope = %q", got)
	}
	counts := agentCounts([]any{map[string]any{"expires": float64(now + 1)}, map[string]any{"expires": float64(now + 1), "paused": true}})
	if counts["active"] != 1 || counts["paused"] != 1 || counts["revoked"] != 0 {
		t.Errorf("counts = %v", counts)
	}
	callbacks := []any{
		map[string]any{"owner": agentActive, "paused": false, "filter": map[string]any{"kinds": []any{float64(1621), float64(1111)}}},
		map[string]any{"owner": agentActive, "paused": true, "filter": map[string]any{}},
		map[string]any{"owner": agentPaused, "paused": false},
	}
	if mine := callbacksFor(callbacks, agentActive); len(mine) != 2 || callbackKinds(mine[0]) != "1621, 1111" || callbackKinds(mine[1]) != "" || callbackState(mine[1]) != "paused" {
		t.Errorf("callbacksFor = %v", mine)
	}
	if counts := callbackCounts(callbacks); counts["active"] != 2 || counts["paused"] != 1 {
		t.Errorf("callback counts = %v", counts)
	}
}

func TestAgentsPageEditPrefillsTheGrantForm(t *testing.T) {
	backend := &agentsBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return backend.policy.Owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/manage/agents?agent="+agentActive+"&edit="+agentActive, nil))
	body := recorder.Body.String()
	for _, marker := range []string{
		`<h3 id="grant">Replace the grant for hermes</h3>`,
		`<input name="name" maxlength="64" required placeholder="release-notes" value="hermes">`,
		`<option value="paste" selected>paste a public key</option>`,
		`<input name="pubkey" pattern="[0-9a-f]{64}" placeholder="64 hex characters" value="` + agentActive + `">`,
		`placeholder="build, agents" value="build, agents">`,
		`>` + strings.Repeat("a", 64) + `:tinyrelay:maintain</textarea>`,
		`<option value="propose" selected>propose</option>`,
		`<textarea name="sites" placeholder="npub1... ttl=30 encrypted&#10;* ttl=7">` + agentSite + ` ttl=30 encrypted
*</textarea>`,
		`placeholder="1, 1111, 1621" value="9, 1111, 1621">`,
		`<button>Sign the replacement</button>`,
		`&amp;edit=` + agentActive + `#grant">edit</a>`,
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("edit form missing %q", marker)
		}
	}
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/manage/agents?edit=nonsense", nil))
	if body := recorder.Body.String(); !strings.Contains(body, `<h3 id="grant">New agent</h3>`) || strings.Contains(body, "selected>paste") {
		t.Fatal("unknown edit key did not fall back to the blank form")
	}
}

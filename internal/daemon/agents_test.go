package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
)

const (
	testOwnerSecret  = "0000000000000000000000000000000000000000000000000000000000000001"
	testAgentSecret  = "0000000000000000000000000000000000000000000000000000000000000002"
	testMemberSecret = "0000000000000000000000000000000000000000000000000000000000000003"
	testModSecret    = "0000000000000000000000000000000000000000000000000000000000000004"
)

func signedEvent(t *testing.T, secret string, kind int, createdAt int64, tags [][]string, content string) event.Event {
	t.Helper()
	if tags == nil {
		tags = [][]string{}
	}
	e := event.Event{Kind: kind, CreatedAt: createdAt, Tags: tags, Content: content}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	return e
}

func agentGrantEvent(t *testing.T, agent string, createdAt, expires int64, extra ...[]string) event.Event {
	t.Helper()
	tags := [][]string{{"d", agent}, {"p", agent}, {"name", "helper"}, {"expiration", strconv.FormatInt(expires, 10)}}
	return signedEvent(t, testOwnerSecret, event.KIND_AGENT_GRANT, createdAt, append(tags, extra...), "")
}

func publishAs(t *testing.T, tenant *Tenant, e event.Event) error {
	t.Helper()
	_, err := tenant.Publish(context.Background(), e, relay.Session{PubKeys: []string{e.PubKey}})
	return err
}

func TestAgentGrantPublishCreatesRoleRowAndRevokesOnDeletion(t *testing.T) {
	app, tenant := testTenant(t)
	ctx := context.Background()
	agent, _ := event.PublicKey(testAgentSecret)
	now := time.Now().Unix()
	grant := agentGrantEvent(t, agent, now, now+3600, []string{"k", "1"})
	if err := publishAs(t, tenant, grant); err != nil {
		t.Fatalf("grant rejected: %v", err)
	}
	if role, _ := tenant.community.Role(ctx, agent); role != "agent" {
		t.Fatalf("role = %q, want agent", role)
	}
	var owner, eventID, name string
	var expires int64
	if err := tenant.store.DB().QueryRow(`SELECT owner,event_id,name,expires_at FROM agent_grants WHERE agent=?`, agent).Scan(&owner, &eventID, &name, &expires); err != nil {
		t.Fatal(err)
	}
	if owner != tenant.Policy().Owner || eventID != grant.ID || name != "helper" || expires != now+3600 {
		t.Fatalf("grant row owner=%s event=%s name=%s expires=%d", owner, eventID, name, expires)
	}
	if err := publishAs(t, tenant, signedEvent(t, testAgentSecret, 1, now, nil, "hello")); err != nil {
		t.Fatalf("agent note rejected: %v", err)
	}
	if err := publishAs(t, tenant, signedEvent(t, testAgentSecret, 7, now, nil, "+")); err == nil || !strings.HasPrefix(err.Error(), "restricted: agent grant does not allow kind 7") {
		t.Fatalf("agent reaction: %v", err)
	}
	records, err := tenant.records.PublishMembership(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	labelled := map[int]bool{}
	for _, e := range records {
		for _, tag := range e.Tags {
			if len(tag) == 3 && tag[1] == agent && tag[2] == "agent" && (e.Kind == event.KIND_ROSTER || e.Kind == event.KIND_GROUP_MEMBERS) {
				labelled[e.Kind] = true
			}
			if e.Kind == event.KIND_ROLE_DEF && tag[0] == "d" && tag[1] == "agent" {
				labelled[e.Kind] = true
			}
		}
	}
	if !labelled[event.KIND_ROSTER] || !labelled[event.KIND_GROUP_MEMBERS] || !labelled[event.KIND_ROLE_DEF] {
		t.Fatalf("agent role missing from records: %v", labelled)
	}
	found := false
	for _, capability := range tenant.Capabilities(nil) {
		if capability.ID == "agents" && capability.Status == "enabled" {
			found = true
		}
	}
	if !found {
		t.Fatal("agents capability not advertised")
	}
	req := httptest.NewRequest(http.MethodGet, "http://relay.test/", nil)
	req.Header.Set("Accept", "application/nostr+json")
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)
	var info map[string]json.RawMessage
	if err := json.Unmarshal(res.Body.Bytes(), &info); err != nil || res.Code != 200 {
		t.Fatalf("info %d %s %v", res.Code, res.Body.String(), err)
	}
	if !strings.Contains(string(info["agents"]), `"id":"agents"`) {
		t.Fatalf("NIP-11 agents entry = %s", info["agents"])
	}
	deletion := signedEvent(t, testOwnerSecret, event.KIND_DELETION, now+1, [][]string{{"a", "30392:" + tenant.Policy().Owner + ":" + agent}}, "")
	if err := publishAs(t, tenant, deletion); err != nil {
		t.Fatalf("deletion rejected: %v", err)
	}
	if role, _ := tenant.community.Role(ctx, agent); role != "" {
		t.Fatalf("role after deletion = %q", role)
	}
	if err := publishAs(t, tenant, signedEvent(t, testAgentSecret, 1, now+2, nil, "again")); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked agent note: %v", err)
	}
	if err := publishAs(t, tenant, agentGrantEvent(t, agent, now+3, now+3600, []string{"k", "1"})); err != nil {
		t.Fatalf("new grant rejected: %v", err)
	}
	if err := publishAs(t, tenant, signedEvent(t, testAgentSecret, 1, now+4, nil, "back")); err != nil {
		t.Fatalf("re-granted agent rejected: %v", err)
	}
	if err := publishAs(t, tenant, agentGrantEvent(t, agent, now+5, now-1, []string{"k", "1"})); err != nil {
		t.Fatalf("expired grant rejected: %v", err)
	}
	if role, _ := tenant.community.Role(ctx, agent); role != "" {
		t.Fatalf("role after expired grant = %q", role)
	}
	var revoked int64
	if err := tenant.store.DB().QueryRow(`SELECT revoked_at FROM agent_grants WHERE agent=?`, agent).Scan(&revoked); err != nil || revoked == 0 {
		t.Fatalf("expired grant not revoked: %d %v", revoked, err)
	}
	if found := capabilityIDs(tenant)["agents"]; found {
		t.Fatal("agents capability advertised without an active agent")
	}
}

func capabilityIDs(tenant *Tenant) map[string]bool {
	out := map[string]bool{}
	for _, capability := range tenant.Capabilities(nil) {
		out[capability.ID] = true
	}
	return out
}

func TestAgentManagementMethodsAndPermissions(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	agent, _ := event.PublicKey(testAgentSecret)
	member, _ := event.PublicKey(testMemberSecret)
	moderator, _ := event.PublicKey(testModSecret)
	now := time.Now().Unix()
	for _, target := range []struct {
		pubkey, role string
	}{{member, "member"}, {moderator, "moderator"}} {
		raw, _ := json.Marshal(map[string]string{"role": target.role})
		if _, err := tenant.Execute(ctx, owner, "setmember", []json.RawMessage{json.RawMessage(strconv.Quote(target.pubkey)), raw}); err != nil {
			t.Fatal(err)
		}
	}
	if err := publishAs(t, tenant, agentGrantEvent(t, agent, now, now+3600, []string{"k", "1"})); err != nil {
		t.Fatal(err)
	}
	methods, err := tenant.Execute(ctx, owner, "supportedmethods", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"listagents", "pauseagent", "resumeagent", "revokeagent", "pauseallagents", "resumeallagents"} {
		if !containsString(methods.([]string), name) {
			t.Fatalf("supportedmethods lacks %s", name)
		}
	}
	param := []json.RawMessage{json.RawMessage(strconv.Quote(agent))}
	for _, actor := range []string{member, agent, strings.Repeat("e", 64)} {
		for _, method := range []string{"listagents", "pauseagent", "revokeagent", "pauseallagents"} {
			if _, err := tenant.Execute(ctx, actor, method, param); err == nil || !strings.HasPrefix(err.Error(), "restricted:") {
				t.Fatalf("%s by non-admin: %v", method, err)
			}
		}
	}
	listed, err := tenant.Execute(ctx, moderator, "listagents", nil)
	if err != nil {
		t.Fatalf("moderator listagents: %v", err)
	}
	agents := listed.([]community.AgentSummary)
	if len(agents) != 1 || agents[0].Agent != agent || agents[0].Name != "helper" || agents[0].Owner != owner || agents[0].Paused || agents[0].RevokedAt != 0 || agents[0].Scope.Kinds[0] != 1 {
		t.Fatalf("listagents = %+v", agents)
	}
	if err := publishAs(t, tenant, signedEvent(t, testAgentSecret, 1, now, nil, "hello")); err != nil {
		t.Fatal(err)
	}
	listed, _ = tenant.Execute(ctx, owner, "listagents", nil)
	if last := listed.([]community.AgentSummary)[0].LastEvent; last != now {
		t.Fatalf("lastEvent = %d, want %d", last, now)
	}
	if _, err := tenant.Execute(ctx, moderator, "pauseagent", param); err != nil {
		t.Fatal(err)
	}
	if err := publishAs(t, tenant, signedEvent(t, testAgentSecret, 1, now+1, nil, "paused")); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("paused agent note: %v", err)
	}
	if _, err := tenant.Execute(ctx, owner, "resumeagent", param); err != nil {
		t.Fatal(err)
	}
	if err := publishAs(t, tenant, signedEvent(t, testAgentSecret, 1, now+2, nil, "resumed")); err != nil {
		t.Fatalf("resumed agent note: %v", err)
	}
	result, err := tenant.Execute(ctx, owner, "pauseallagents", nil)
	if err != nil || result.(map[string]any)["changed"] != 1 {
		t.Fatalf("pauseallagents = %v %v", result, err)
	}
	if err := publishAs(t, tenant, signedEvent(t, testAgentSecret, 1, now+3, nil, "all paused")); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("note during pause: %v", err)
	}
	if result, err := tenant.Execute(ctx, owner, "resumeallagents", nil); err != nil || result.(map[string]any)["changed"] != 1 {
		t.Fatalf("resumeallagents = %v %v", result, err)
	}
	if _, err := tenant.Execute(ctx, owner, "revokeagent", param); err != nil {
		t.Fatal(err)
	}
	if role, _ := tenant.community.Role(ctx, agent); role != "" {
		t.Fatalf("role after revoke = %q", role)
	}
	if err := publishAs(t, tenant, signedEvent(t, testAgentSecret, 1, now+4, nil, "revoked")); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked agent note: %v", err)
	}
	var stored int
	if err := tenant.store.DB().QueryRow(`SELECT count(*) FROM events WHERE kind=? AND d=?`, event.KIND_AGENT_GRANT, agent).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("grant event kept for audit: %d %v", stored, err)
	}
	if _, err := tenant.Execute(ctx, owner, "pauseagent", param); err == nil {
		t.Fatal("paused a revoked agent")
	}
	if _, err := tenant.Execute(ctx, owner, "revokeagent", []json.RawMessage{json.RawMessage(strconv.Quote(strings.Repeat("f", 64)))}); err == nil {
		t.Fatal("revoked an unknown agent")
	}
	audit, err := tenant.Execute(ctx, owner, "listaudit", nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range audit.([]community.AuditRow) {
		seen[row.Action] = true
	}
	for _, action := range []string{"grantagent", "pauseagent", "resumeagent", "pauseallagents", "resumeallagents", "revokeagent"} {
		if !seen[action] {
			t.Fatalf("audit lacks %s: %v", action, seen)
		}
	}
}

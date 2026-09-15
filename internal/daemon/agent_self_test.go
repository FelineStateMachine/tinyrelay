package daemon

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func browseGrantAs(t *testing.T, tenant *Tenant, actor, params string) grantSelf {
	t.Helper()
	var raw []json.RawMessage
	if params != "" {
		raw = []json.RawMessage{json.RawMessage(params)}
	}
	result, err := tenant.Execute(context.Background(), actor, "browsegrant", raw)
	if err != nil {
		t.Fatalf("browsegrant by %q: %v", actor, err)
	}
	self, ok := result.(grantSelf)
	if !ok {
		t.Fatalf("browsegrant result %T", result)
	}
	return self
}

func TestBrowseGrantReportsOwnGrantWithoutOperatorRole(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	agent, _ := event.PublicKey(testAgentSecret)
	member, _ := event.PublicKey(testMemberSecret)
	now := time.Now().Unix()
	raw, _ := json.Marshal(map[string]string{"role": "member"})
	if _, err := tenant.Execute(ctx, owner, "setmember", []json.RawMessage{json.RawMessage(strconv.Quote(member)), raw}); err != nil {
		t.Fatal(err)
	}
	if err := publishAs(t, tenant, agentGrantEvent(t, agent, now, now+3600, []string{"k", "9"}, []string{"k", "1"}, []string{"room", "build"}, []string{"jobs", "serve"}, []string{"rate", "30"})); err != nil {
		t.Fatal(err)
	}
	if err := publishAs(t, tenant, signedEvent(t, testAgentSecret, 1, now-5, nil, "hello")); err != nil {
		t.Fatal(err)
	}
	needs := `{"rooms":["build","ops"],"kinds":[9,12,43002,43001,0]}`

	// The agent sees its own grant, state and what its needs lack.
	self := browseGrantAs(t, tenant, agent, needs)
	if !self.Member || self.Role != "agent" || self.State != "active" || !self.Enforced || self.Grant == nil || self.Grant.Name != "helper" || self.Grant.LastEvent != now-5 {
		t.Fatalf("agent self = %+v", self)
	}
	if self.Rate != 30 || self.Expires != now+3600 || self.Allows == nil || strings.Join(self.Allows.Rooms, ",") != "build" || self.Allows.Jobs != "serve" || len(self.Allows.Kinds) != 2 || self.Allows.Kinds[0] != 9 {
		t.Fatalf("agent allows = %+v rate %d expires %d", self.Allows, self.Rate, self.Expires)
	}
	if self.Missing == nil || strings.Join(self.Missing.Rooms, ",") != "ops" || len(self.Missing.Kinds) != 2 || self.Missing.Kinds[0] != 12 || self.Missing.Kinds[1] != 43001 {
		t.Fatalf("agent missing = %+v", self.Missing)
	}
	if encoded, _ := json.Marshal(self); !strings.Contains(string(encoded), `"grant":{`) || !strings.Contains(string(encoded), `"allows":{`) {
		t.Fatalf("wire shape %s", encoded)
	}

	// Without needs there is no missing section.
	if self := browseGrantAs(t, tenant, agent, ""); self.Missing != nil || self.State != "active" {
		t.Fatalf("agent without needs = %+v", self)
	}

	// A human member has no grant and the gate does not apply one, so nothing
	// is missing for it.
	self = browseGrantAs(t, tenant, member, needs)
	if !self.Member || self.Role != "member" || self.State != "none" || self.Grant != nil || self.Allows != nil || self.Enforced {
		t.Fatalf("member self = %+v", self)
	}
	if self.Missing == nil || len(self.Missing.Rooms) != 0 || len(self.Missing.Kinds) != 0 {
		t.Fatalf("member missing = %+v", self.Missing)
	}
	if encoded, _ := json.Marshal(self); !strings.Contains(string(encoded), `"grant":null`) || strings.Contains(string(encoded), `"allows"`) {
		t.Fatalf("member wire shape %s", encoded)
	}

	// A stranger is not a member and lacks everything it asked for.
	stranger := strings.Repeat("e", 64)
	self = browseGrantAs(t, tenant, stranger, needs)
	if self.Member || self.Role != "" || self.State != "none" || self.Grant != nil || !self.Enforced {
		t.Fatalf("stranger self = %+v", self)
	}
	if self.Missing == nil || strings.Join(self.Missing.Rooms, ",") != "build,ops" || len(self.Missing.Kinds) != 5 {
		t.Fatalf("stranger missing = %+v", self.Missing)
	}

	// The owner may still not use it to read another key's grant: the
	// method only ever describes the caller.
	if self := browseGrantAs(t, tenant, owner, `{"agent":"`+agent+`"}`); self.Role != "owner" || self.Grant != nil {
		t.Fatalf("owner self = %+v", self)
	}

	// Paused and revoked grants are reported as such.
	if _, err := tenant.Execute(ctx, owner, "pauseagent", []json.RawMessage{json.RawMessage(strconv.Quote(agent))}); err != nil {
		t.Fatal(err)
	}
	if self := browseGrantAs(t, tenant, agent, needs); self.State != "paused" || !self.Member || len(self.Missing.Rooms) != 1 {
		t.Fatalf("paused self = %+v", self)
	}
	if _, err := tenant.Execute(ctx, owner, "revokeagent", []json.RawMessage{json.RawMessage(strconv.Quote(agent))}); err != nil {
		t.Fatal(err)
	}
	if self := browseGrantAs(t, tenant, agent, ""); self.State != "revoked" || self.Member || self.Role != "" {
		t.Fatalf("revoked self = %+v", self)
	}

	// Bad needs are refused.
	for _, bad := range []string{`{"kinds":[-1]}`, `{"rooms":[""]}`, `[1]`} {
		if _, err := tenant.Execute(ctx, agent, "browsegrant", []json.RawMessage{json.RawMessage(bad)}); err == nil || !strings.HasPrefix(err.Error(), "invalid:") {
			t.Fatalf("browsegrant %s: %v", bad, err)
		}
	}
}

func TestBrowseGrantReportsExpiredGrants(t *testing.T) {
	_, tenant := testTenant(t)
	agent, _ := event.PublicKey(testAgentSecret)
	now := time.Now().Unix()
	if err := publishAs(t, tenant, agentGrantEvent(t, agent, now-10, now+60, []string{"k", "9"})); err != nil {
		t.Fatal(err)
	}
	if self := browseGrantAs(t, tenant, agent, ""); self.State != "active" {
		t.Fatalf("fresh grant = %+v", self)
	}
	if _, err := tenant.store.DB().Exec(`UPDATE agent_grants SET expires_at=? WHERE agent=?`, now-1, agent); err != nil {
		t.Fatal(err)
	}
	if self := browseGrantAs(t, tenant, agent, `{"kinds":[9]}`); self.State != "expired" || self.Grant == nil || len(self.Missing.Kinds) != 0 {
		t.Fatalf("expired grant = %+v", self)
	}
}

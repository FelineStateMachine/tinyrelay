package gates

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

const (
	agentOwnerSecret = "0000000000000000000000000000000000000000000000000000000000000001"
	agentSecret      = "0000000000000000000000000000000000000000000000000000000000000002"
	agentOtherSecret = "0000000000000000000000000000000000000000000000000000000000000003"
	agentNow         = int64(1_700_000_000)
)

type agentFixture struct {
	gate      *Gate
	community *community.Service
	store     *storage.Store
	owner     string
	agent     string
}

func newAgentFixture(t *testing.T) agentFixture {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner, _ := event.PublicKey(agentOwnerSecret)
	agent, _ := event.PublicKey(agentSecret)
	svc, err := community.New(ctx, store, owner)
	if err != nil {
		t.Fatal(err)
	}
	p := policy.Defaults(owner)
	gate, err := New(Config{Store: store, Community: svc, Policy: func() policy.Policy { return p }, Slug: "general"})
	if err != nil {
		t.Fatal(err)
	}
	return agentFixture{gate: gate, community: svc, store: store, owner: owner, agent: agent}
}

func signed(t *testing.T, secret string, kind int, createdAt int64, tags [][]string, content string) event.Event {
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

func grantTags(agent string, expires int64, extra ...[]string) [][]string {
	tags := [][]string{{"d", agent}, {"p", agent}, {"name", "helper"}, {"expiration", strconv.FormatInt(expires, 10)}}
	return append(tags, extra...)
}

// grant publishes a grant through the gate and mirrors it as the tenant
// would. Later grants for the same agent need a later createdAt to replace
// the earlier one.
func (f agentFixture) grant(t *testing.T, createdAt int64, tags [][]string) event.Event {
	t.Helper()
	ctx := context.Background()
	e := signed(t, agentOwnerSecret, event.KIND_AGENT_GRANT, createdAt, tags, "")
	if err := f.gate.Write(ctx, e, relay.Session{PubKeys: []string{f.owner}}, agentNow); err != nil {
		t.Fatalf("grant rejected: %v", err)
	}
	if err := f.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := storage.SaveTx(ctx, tx, e, storage.SaveOptions{Now: agentNow}); err != nil {
			return err
		}
		return f.community.ApplyAgentEventTx(ctx, tx, e, agentNow)
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

func expectRestricted(t *testing.T, err error, fragment string) {
	t.Helper()
	if err == nil || !strings.HasPrefix(err.Error(), "restricted: agent grant") || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("want restricted agent grant error containing %q, got %v", fragment, err)
	}
}

func TestAgentGrantScopesKindsRoomsAndRepositories(t *testing.T) {
	f := newAgentFixture(t)
	ctx := context.Background()
	repo := f.owner + ":project"
	f.grant(t, agentNow, grantTags(f.agent, agentNow+3600, []string{"room", "general"}, []string{"repo", repo + ":read"}, []string{"k", "1"}, []string{"k", "1621"}, []string{"k", "1630"}, []string{"k", "1111"}))
	if role, _ := f.community.Role(ctx, f.agent); role != "agent" {
		t.Fatalf("role = %q, want agent", role)
	}
	session := relay.Session{PubKeys: []string{f.agent}}
	if err := f.gate.Write(ctx, signed(t, agentSecret, 1, agentNow, nil, "hello"), session, agentNow); err != nil {
		t.Fatalf("allowed kind rejected: %v", err)
	}
	if err := f.gate.Write(ctx, signed(t, agentSecret, 0, agentNow, nil, "{}"), session, agentNow); err != nil {
		t.Fatalf("profile rejected: %v", err)
	}
	expectRestricted(t, f.gate.Write(ctx, signed(t, agentSecret, 7, agentNow, nil, "+"), session, agentNow), "kind 7")
	if err := f.gate.Write(ctx, signed(t, agentSecret, 1, agentNow, [][]string{{"h", "general"}}, "in room"), session, agentNow); err != nil {
		t.Fatalf("allowed room rejected: %v", err)
	}
	expectRestricted(t, f.gate.Write(ctx, signed(t, agentSecret, 1, agentNow, [][]string{{"h", "other"}}, "wrong room"), session, agentNow), "room other")
	address := "30617:" + repo
	if err := f.gate.Write(ctx, signed(t, agentSecret, 1621, agentNow, [][]string{{"a", address}}, "issue"), session, agentNow); err != nil {
		t.Fatalf("issue in readable repository rejected: %v", err)
	}
	if err := f.gate.Write(ctx, signed(t, agentSecret, 1111, agentNow, [][]string{{"A", address}}, "comment"), session, agentNow); err != nil {
		t.Fatalf("comment in readable repository rejected: %v", err)
	}
	expectRestricted(t, f.gate.Write(ctx, signed(t, agentSecret, 1630, agentNow, [][]string{{"a", address}}, "status"), session, agentNow), "status changes")
	expectRestricted(t, f.gate.Write(ctx, signed(t, agentSecret, 1621, agentNow, [][]string{{"a", "30617:" + f.owner + ":elsewhere"}}, "issue"), session, agentNow), "repository")
	expectRestricted(t, f.gate.Write(ctx, signed(t, agentSecret, 1621, agentNow, nil, "issue"), session, agentNow), "repository address")
	f.grant(t, agentNow+1, grantTags(f.agent, agentNow+3600, []string{"repo", repo + ":maintain"}, []string{"k", "1630"}))
	if err := f.gate.Write(ctx, signed(t, agentSecret, 1630, agentNow, [][]string{{"a", address}}, "status"), session, agentNow); err != nil {
		t.Fatalf("maintainer status rejected: %v", err)
	}
	// Wiki access carries the wiki kinds without listing them.
	f.grant(t, agentNow+2, grantTags(f.agent, agentNow+3600, []string{"wiki", "propose"}))
	for _, kind := range []int{event.KIND_WIKI_ARTICLE, event.KIND_WIKI_MERGE} {
		if err := f.gate.Write(ctx, signed(t, agentSecret, kind, agentNow, [][]string{{"d", "notes"}, {"title", "Notes"}, {"a", "30818:" + f.owner + ":notes"}, {"p", f.owner}, {"e", strings.Repeat("e", 64), "", "source"}}, "wiki"), session, agentNow); err != nil {
			t.Fatalf("wiki kind %d rejected for propose: %v", kind, err)
		}
	}
	expectRestricted(t, f.gate.Write(ctx, signed(t, agentSecret, event.KIND_WIKI_REDIRECT, agentNow, [][]string{{"d", "notes"}}, ""), session, agentNow), "kind 30819")
	f.grant(t, agentNow+3, grantTags(f.agent, agentNow+3600, []string{"wiki", "edit"}))
	if err := f.gate.Write(ctx, signed(t, agentSecret, event.KIND_WIKI_REDIRECT, agentNow, [][]string{{"d", "notes"}, {"redirect", "30818:" + f.owner + ":notes"}}, ""), session, agentNow); err != nil {
		t.Fatalf("redirect rejected for edit: %v", err)
	}
}

func TestAgentGrantStateRejectsWrites(t *testing.T) {
	f := newAgentFixture(t)
	ctx := context.Background()
	session := relay.Session{PubKeys: []string{f.agent}}
	note := signed(t, agentSecret, 1, agentNow, nil, "hello")
	f.grant(t, agentNow, grantTags(f.agent, agentNow+60, []string{"k", "1"}))
	if err := f.gate.Write(ctx, note, session, agentNow); err != nil {
		t.Fatalf("active grant rejected: %v", err)
	}
	expectRestricted(t, f.gate.Write(ctx, note, session, agentNow+61), "expired")
	if _, err := f.community.SetAgentPaused(ctx, f.owner, f.agent, true); err != nil {
		t.Fatal(err)
	}
	expectRestricted(t, f.gate.Write(ctx, note, session, agentNow), "paused")
	if _, err := f.community.SetAgentPaused(ctx, f.owner, f.agent, false); err != nil {
		t.Fatal(err)
	}
	if err := f.gate.Write(ctx, note, session, agentNow); err != nil {
		t.Fatalf("resumed grant rejected: %v", err)
	}
	if _, err := f.community.RevokeAgent(ctx, f.owner, f.agent, agentNow); err != nil {
		t.Fatal(err)
	}
	expectRestricted(t, f.gate.Write(ctx, note, session, agentNow), "revoked")
	if role, _ := f.community.Role(ctx, f.agent); role != "" {
		t.Fatalf("revoked agent still has role %q", role)
	}
	f.grant(t, agentNow+1, grantTags(f.agent, agentNow-1, []string{"k", "1"}))
	expectRestricted(t, f.gate.Write(ctx, note, session, agentNow), "revoked")
}

func TestAgentGrantRateLimitSlidesOverOneMinute(t *testing.T) {
	f := newAgentFixture(t)
	ctx := context.Background()
	session := relay.Session{PubKeys: []string{f.agent}}
	f.grant(t, agentNow, grantTags(f.agent, agentNow+3600, []string{"k", "1"}, []string{"rate", "2"}))
	for i := 0; i < 2; i++ {
		if err := f.gate.Write(ctx, signed(t, agentSecret, 1, agentNow+int64(i), nil, "n"), session, agentNow+int64(i)); err != nil {
			t.Fatalf("event %d rejected: %v", i, err)
		}
	}
	expectRestricted(t, f.gate.Write(ctx, signed(t, agentSecret, 1, agentNow+2, nil, "n"), session, agentNow+2), "2 events per minute")
	if err := f.gate.Write(ctx, signed(t, agentSecret, 1, agentNow+61, nil, "n"), session, agentNow+61); err != nil {
		t.Fatalf("event after the window rejected: %v", err)
	}
}

func TestHumanMemberWithGrantKeepsMemberPermissions(t *testing.T) {
	f := newAgentFixture(t)
	ctx := context.Background()
	raw, _ := json.Marshal(f.agent)
	if _, err := f.community.Execute(ctx, f.owner, "setmember", []json.RawMessage{raw}); err != nil {
		t.Fatal(err)
	}
	f.grant(t, agentNow, grantTags(f.agent, agentNow+3600, []string{"k", "1"}))
	if role, _ := f.community.Role(ctx, f.agent); role != "member" {
		t.Fatalf("grant changed a human role to %q", role)
	}
	if err := f.gate.Write(ctx, signed(t, agentSecret, 7, agentNow, nil, "+"), relay.Session{PubKeys: []string{f.agent}}, agentNow); err != nil {
		t.Fatalf("member constrained by grant: %v", err)
	}
}

func TestImportEnforcesAgentGrant(t *testing.T) {
	f := newAgentFixture(t)
	ctx := context.Background()
	f.grant(t, agentNow, grantTags(f.agent, agentNow+3600, []string{"k", "1"}))
	if err := f.gate.Import(ctx, signed(t, agentSecret, 1, agentNow, nil, "synced"), agentNow); err != nil {
		t.Fatalf("allowed import rejected: %v", err)
	}
	expectRestricted(t, f.gate.Import(ctx, signed(t, agentSecret, 7, agentNow, nil, "+"), agentNow), "kind 7")
}

func TestAgentGrantShapeAndSigner(t *testing.T) {
	f := newAgentFixture(t)
	ctx := context.Background()
	other, _ := event.PublicKey(agentOtherSecret)
	cases := []struct {
		name   string
		secret string
		tags   [][]string
		want   string
	}{
		{"stranger signer", agentOtherSecret, grantTags(f.agent, agentNow+60), "restricted: only the owner or a moderator"},
		{"missing expiration", agentOwnerSecret, [][]string{{"d", f.agent}, {"p", f.agent}}, "invalid: agent grant needs an expiration"},
		{"too far out", agentOwnerSecret, grantTags(f.agent, agentNow+366*86400), "invalid: agent grant expiration"},
		{"mismatched p", agentOwnerSecret, [][]string{{"d", f.agent}, {"p", other}, {"expiration", "1700003600"}}, "invalid: agent grant p tag"},
		{"self grant", agentOwnerSecret, grantTags(f.owner, agentNow+60), "invalid: agent grant may not name its signer"},
		{"bad repo", agentOwnerSecret, grantTags(f.agent, agentNow+60, []string{"repo", "nope"}), "invalid: agent grant repo tag"},
		{"bad rate", agentOwnerSecret, grantTags(f.agent, agentNow+60, []string{"rate", "601"}), "invalid: agent grant rate"},
		{"bad wiki", agentOwnerSecret, grantTags(f.agent, agentNow+60, []string{"wiki", "admin"}), "invalid: agent grant wiki tag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := signed(t, tc.secret, event.KIND_AGENT_GRANT, agentNow, tc.tags, "")
			err := f.gate.Write(ctx, e, relay.Session{PubKeys: []string{e.PubKey}}, agentNow)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	e := signed(t, agentOwnerSecret, event.KIND_AGENT_GRANT, agentNow, grantTags(f.agent, agentNow+60, []string{"k", "1"}, []string{"wiki", "propose"}), `{"note":"ok"}`)
	if err := f.gate.Write(ctx, e, relay.Session{PubKeys: []string{f.owner}}, agentNow); err != nil {
		t.Fatalf("valid grant rejected: %v", err)
	}
	grant, err := community.ParseAgentGrant(e, agentNow)
	if err != nil || grant.Scope.Rate != community.AgentRateDefault || grant.Scope.Wiki != "propose" || grant.Name != "helper" {
		t.Fatalf("parsed grant %+v %v", grant, err)
	}
}

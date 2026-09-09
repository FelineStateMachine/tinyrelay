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
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
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

func TestAgentGrantSitesCoverLabelsAndRequireExpiry(t *testing.T) {
	f := newAgentFixture(t)
	ctx := context.Background()
	session := relay.Session{PubKeys: []string{f.agent}}
	hash := strings.Repeat("a", 64)
	paths := [][]string{{"path", "/index.html", hash}}
	own := sites.SiteLabel(event.Event{Kind: sites.KindSite, PubKey: f.agent})
	base, _ := sites.Base36(f.agent)
	named := base + "docs"
	manifest := func(kind int, tags ...[]string) event.Event {
		return signed(t, agentSecret, kind, agentNow, append(append([][]string{}, paths...), tags...), "")
	}
	// Without a sites tag the manifest kinds stay closed.
	f.grant(t, agentNow, grantTags(f.agent, agentNow+3600, []string{"k", "1"}))
	expectRestricted(t, f.gate.Write(ctx, manifest(sites.KindSite), session, agentNow), "kind 15128")
	// One label without a ttl: that site only, no expiration needed.
	f.grant(t, agentNow+1, grantTags(f.agent, agentNow+3600, []string{"sites", named}))
	if err := f.gate.Write(ctx, manifest(sites.KindNamedSite, []string{"d", "docs"}), session, agentNow); err != nil {
		t.Fatalf("covered named site rejected: %v", err)
	}
	expectRestricted(t, f.gate.Write(ctx, manifest(sites.KindNamedSite, []string{"d", "blog"}), session, agentNow), "site "+base+"blog")
	expectRestricted(t, f.gate.Write(ctx, manifest(sites.KindSite), session, agentNow), "site "+own)
	// A ttl requires an expiration tag within it; the reason says what to add.
	f.grant(t, agentNow+2, grantTags(f.agent, agentNow+3600, []string{"sites", own, "ttl=2"}, []string{"sites", named, "ttl=1", "encrypted"}))
	latest := strconv.FormatInt(agentNow+2*86400, 10)
	err := f.gate.Write(ctx, manifest(sites.KindSite), session, agentNow)
	if err == nil || !strings.HasPrefix(err.Error(), "invalid: agent grant allows site "+own+" for 2 days: add an expiration tag no later than "+latest) {
		t.Fatalf("manifest without expiration: %v", err)
	}
	err = f.gate.Write(ctx, manifest(sites.KindSite, []string{"expiration", strconv.FormatInt(agentNow+3*86400, 10)}), session, agentNow)
	if err == nil || !strings.Contains(err.Error(), "set the expiration tag no later than "+latest) {
		t.Fatalf("manifest expiring too late: %v", err)
	}
	if err := f.gate.Write(ctx, manifest(sites.KindSite, []string{"expiration", latest}), session, agentNow); err != nil {
		t.Fatalf("manifest within the ttl rejected: %v", err)
	}
	if err := f.gate.Write(ctx, manifest(sites.KindNamedSite, []string{"d", "docs"}, []string{"expiration", strconv.FormatInt(agentNow+86400, 10)}), session, agentNow); err != nil {
		t.Fatalf("named manifest within its own ttl rejected: %v", err)
	}
	if err := f.gate.Write(ctx, manifest(sites.KindNamedSite, []string{"d", "docs"}, []string{"expiration", latest}), session, agentNow); err == nil {
		t.Fatal("named manifest beyond its own ttl accepted")
	}
	// A star covers every label under the key and the longest ttl applies.
	grant := f.grant(t, agentNow+3, grantTags(f.agent, agentNow+3600, []string{"sites", "*", "ttl=5"}, []string{"sites", named, "ttl=1"}))
	if err := f.gate.Write(ctx, manifest(sites.KindNamedSite, []string{"d", "blog"}, []string{"expiration", strconv.FormatInt(agentNow+5*86400, 10)}), session, agentNow); err != nil {
		t.Fatalf("star label rejected: %v", err)
	}
	if err := f.gate.Write(ctx, manifest(sites.KindNamedSite, []string{"d", "docs"}, []string{"expiration", strconv.FormatInt(agentNow+5*86400, 10)}), session, agentNow); err != nil {
		t.Fatalf("longest ttl not applied: %v", err)
	}
	parsed, err := community.ParseAgentGrant(grant, agentNow)
	if err != nil || parsed.UploadTTLDays() != 5 || parsed.UploadsEncrypted() {
		t.Fatalf("parsed sites %+v %v", parsed.Scope.Sites, err)
	}
	// Snapshots are not admitted by the sites tag.
	expectRestricted(t, f.gate.Write(ctx, manifest(sites.KindSiteSnapshot), session, agentNow), "kind 5128")
	// Paused and revoked grants stop manifests like every other event.
	if _, err := f.community.SetAgentPaused(ctx, f.owner, f.agent, true); err != nil {
		t.Fatal(err)
	}
	expectRestricted(t, f.gate.Write(ctx, manifest(sites.KindSite, []string{"expiration", latest}), session, agentNow), "paused")
	if _, err := f.community.RevokeAgent(ctx, f.owner, f.agent, agentNow); err != nil {
		t.Fatal(err)
	}
	expectRestricted(t, f.gate.Write(ctx, manifest(sites.KindSite, []string{"expiration", latest}), session, agentNow), "revoked")
}

func TestAgentGrantSitesShape(t *testing.T) {
	f := newAgentFixture(t)
	ctx := context.Background()
	other, _ := event.PublicKey(agentOtherSecret)
	own := sites.SiteLabel(event.Event{Kind: sites.KindSite, PubKey: f.agent})
	foreign := sites.SiteLabel(event.Event{Kind: sites.KindSite, PubKey: other})
	for _, tc := range []struct {
		name string
		tag  []string
		want string
	}{
		{"empty", []string{"sites"}, "invalid: agent grant sites tag needs a site label"},
		{"bad label", []string{"sites", "not-a-site"}, "invalid: agent grant sites tag needs a site label"},
		{"other key", []string{"sites", foreign}, "invalid: agent grant sites label must be a site under the agent's own key"},
		{"snapshot", []string{"sites", "v" + strings.Repeat("0", 50)}, "invalid: agent grant sites tag needs a site label"},
		{"ttl zero", []string{"sites", "*", "ttl=0"}, "invalid: agent grant sites ttl must be between 1 and 365"},
		{"ttl long", []string{"sites", "*", "ttl=366"}, "invalid: agent grant sites ttl must be between 1 and 365"},
		{"ttl text", []string{"sites", "*", "ttl=soon"}, "invalid: agent grant sites ttl must be between 1 and 365"},
		{"unknown flag", []string{"sites", own, "public"}, "invalid: agent grant sites tag allows only ttl=<days> and encrypted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := signed(t, agentOwnerSecret, event.KIND_AGENT_GRANT, agentNow, grantTags(f.agent, agentNow+60, tc.tag), "")
			err := f.gate.Write(ctx, e, relay.Session{PubKeys: []string{f.owner}}, agentNow)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	e := signed(t, agentOwnerSecret, event.KIND_AGENT_GRANT, agentNow, grantTags(f.agent, agentNow+60, []string{"sites", own, "ttl=30", "encrypted"}, []string{"sites", own, "ttl=7"}, []string{"sites", "*"}), "")
	if err := f.gate.Write(ctx, e, relay.Session{PubKeys: []string{f.owner}}, agentNow); err != nil {
		t.Fatalf("valid sites grant rejected: %v", err)
	}
	grant, err := community.ParseAgentGrant(e, agentNow)
	if err != nil {
		t.Fatal(err)
	}
	want := []community.AgentSite{{Label: own, TTLDays: 30, Encrypted: true}, {Label: "*"}}
	if len(grant.Scope.Sites) != len(want) || grant.Scope.Sites[0] != want[0] || grant.Scope.Sites[1] != want[1] {
		t.Fatalf("sites = %+v, want %+v", grant.Scope.Sites, want)
	}
	if !grant.UploadsEncrypted() || grant.UploadTTLDays() != 0 {
		t.Fatalf("an entry without a ttl should leave uploads unlimited: ttl=%d encrypted=%v", grant.UploadTTLDays(), grant.UploadsEncrypted())
	}
}

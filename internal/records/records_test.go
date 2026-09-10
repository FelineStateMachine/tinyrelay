package records

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/communityread"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type emptyCommunityReader struct{}

func (emptyCommunityReader) Members(context.Context) ([]communityread.Member, error) {
	return nil, nil
}

func (emptyCommunityReader) ModerationCounts(context.Context, int64) (communityread.ModerationCounts, error) {
	return communityread.ModerationCounts{}, nil
}

func (emptyCommunityReader) MemberStatus(context.Context, string) (bool, int, error) {
	return false, 0, nil
}

func testStore(t *testing.T) (*storage.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "tenant.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, ctx
}

func TestIdentitySurvivesRestartAndClockIsMonotonic(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tenant.db")
	s, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	p := policy.Defaults("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	r, err := New(ctx, Config{Community: emptyCommunityReader{}, Store: s, Policy: func() policy.Policy { return p }, RelayURL: "wss://relay.example"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.Generate(ctx, event.KIND_APP_DATA, [][]string{{"d", "x"}}, "one", 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := event.Validate(first); err != nil {
		t.Fatal(err)
	}
	key := r.PublicKey()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := New(ctx, Config{Community: emptyCommunityReader{}, Store: s2, Policy: func() policy.Policy { return p }, RelayURL: "wss://relay.example"})
	if err != nil {
		t.Fatal(err)
	}
	if r2.PublicKey() != key {
		t.Fatalf("identity changed across restart: %s != %s", r2.PublicKey(), key)
	}
	second, err := r2.Generate(ctx, event.KIND_APP_DATA, [][]string{{"d", "x"}}, "two", 1)
	if err != nil {
		t.Fatal(err)
	}
	if second.CreatedAt <= first.CreatedAt {
		t.Fatalf("clock regressed: %d <= %d", second.CreatedAt, first.CreatedAt)
	}
	if err := event.Validate(second); err != nil {
		t.Fatal(err)
	}
	_ = s2.Close()
}

func TestIdentityContinuityAndViewPrivacy(t *testing.T) {
	s, ctx := testStore(t)
	p := policy.Defaults("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	p.Views = map[string]string{"profiles": "daily"}
	owner := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	p.Owner = owner
	r, err := New(ctx, Config{Community: emptyCommunityReader{}, Store: s, Policy: func() policy.Policy { return p }, RelayURL: "wss://relay.example"})
	if err != nil {
		t.Fatal(err)
	}
	key := r.PublicKey()
	p.Reads = "members"
	if _, err := r.Views(ctx, policy.Access{}, 100); err == nil {
		t.Fatal("private view leaked to guest")
	}
	if _, err := r.Views(ctx, policy.Access{Owner: true}, 100); err != nil {
		t.Fatal(err)
	}
	if r.PublicKey() != key {
		t.Fatal("public identity changed")
	}
}

func TestDirectoryPrivateViewsRequireMembersAndDoNotTick(t *testing.T) {
	s, ctx := testStore(t)
	owner := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	p := policy.Defaults(owner)
	p.DirectoryPublic = false
	p.Views = map[string]string{"profiles": "daily"}
	r, err := New(ctx, Config{Community: emptyCommunityReader{}, Store: s, Policy: func() policy.Policy { return p }, RelayURL: "wss://relay.example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Views(ctx, policy.Access{}, 100); err == nil {
		t.Fatal("private directory view leaked to guest")
	}
	if err := r.Tick(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB().QueryRow(`SELECT count(*) FROM events WHERE kind=?`, event.KIND_VIEW).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("private tick stored %d views", count)
	}
}

func TestViewAudienceAndMemberFoldAreNotStored(t *testing.T) {
	s, ctx := testStore(t)
	owner := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	p := policy.Defaults(owner)
	p.DirectoryPublic = false
	p.Reads = "open"
	p.Views = map[string]string{"profiles": "daily", "relays": "off", "calendar": "daily", "moderation": "off", "articles": "off", "zaps": "off", "presence": "off"}
	generated := []event.Event{}
	r, err := New(ctx, Config{Community: emptyCommunityReader{}, Store: s, Policy: func() policy.Policy { return p }, RelayURL: "wss://relay.example", OnGenerated: func(context.Context, event.Event) error {
		generated = append(generated, event.Event{})
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Calendar is read-public even when the directory is private, while the
	// profile fold is member-only and must be signed without persistence.
	views, err := r.Views(ctx, policy.Access{}, 100)
	if err == nil {
		t.Fatal("guest received a member-only profile view")
	}
	if err := r.Tick(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if len(generated) != 1 {
		t.Fatalf("generated callbacks=%d, want public calendar only", len(generated))
	}
	_ = views
}

func TestPresenceIsSignedWithoutGeneratedCallback(t *testing.T) {
	s, ctx := testStore(t)
	owner := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	p := policy.Defaults(owner)
	called := 0
	r, err := New(ctx, Config{Community: emptyCommunityReader{}, Store: s, Policy: func() policy.Policy { return p }, OnGenerated: func(context.Context, event.Event) error {
		called++
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Presence(ctx, policy.Access{Owner: true}, 100); err != nil {
		t.Fatal(err)
	}
	if called != 0 {
		t.Fatalf("presence invoked generated callback %d times", called)
	}
}

func TestProfileFoldUsesMembersAndNIP05(t *testing.T) {
	owner := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	member := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	e := event.Event{PubKey: owner, Kind: event.KIND_PROFILE, CreatedAt: 1, Content: `{"name":"Owner"}`}
	tags, _ := profileFold([]event.Event{e}, "wss://relay.example", map[string]string{owner: "", member: "alice"}, []string{owner, member})
	if len(tags) != 2 || tags[1][1] != member || tags[1][4] != "alice@relay.example" {
		t.Fatalf("profile fold=%v", tags)
	}
}

func TestHourlyViewFingerprintSkipsUnchangedRun(t *testing.T) {
	s, ctx := testStore(t)
	owner := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	p := policy.Defaults(owner)
	p.Views = map[string]string{"profiles": "off", "relays": "off", "calendar": "hourly", "moderation": "off", "articles": "off", "zaps": "off", "presence": "off"}
	generated := 0
	r, err := New(ctx, Config{Community: emptyCommunityReader{}, Store: s, Policy: func() policy.Policy { return p }, OnGenerated: func(context.Context, event.Event) error {
		generated++
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Tick(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if err := r.Tick(ctx, 3700); err != nil {
		t.Fatal(err)
	}
	if generated != 1 {
		t.Fatalf("unchanged hourly view generated %d records", generated)
	}
	runs, err := r.ViewRuns(ctx, "calendar")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs=%d, want only published run", len(runs))
	}
}

func TestDefaultArticleWriteTriggerCoalescesDurably(t *testing.T) {
	s, ctx := testStore(t)
	owner := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	p := policy.Defaults(owner)
	for _, name := range []string{"profiles", "relays", "calendar", "moderation", "zaps", "presence"} {
		p.Views[name] = "off"
	}
	generated := 0
	r, err := New(ctx, Config{Community: emptyCommunityReader{}, Store: s, Policy: func() policy.Policy { return p }, OnGenerated: func(context.Context, event.Event) error { generated++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.MarkView(ctx, "articles", 100); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkView(ctx, "articles", 101); err != nil {
		t.Fatal(err)
	}
	if got, err := r.NextDue(ctx, 105); err != nil || got != 110 {
		t.Fatalf("next due=%d err=%v", got, err)
	}
	if err := r.Tick(ctx, 105); err != nil {
		t.Fatal(err)
	}
	if generated != 0 {
		t.Fatal("published before coalescing window")
	}
	if err := r.Tick(ctx, 110); err != nil {
		t.Fatal(err)
	}
	if generated != 1 {
		t.Fatalf("generated=%d, want one coalesced article view", generated)
	}
	var dirty int64
	if err := s.GetSetting(ctx, "records.view.articles.dirty", &dirty); err != nil || dirty != 0 {
		t.Fatalf("dirty=%d err=%v", dirty, err)
	}
}

func TestSuccessionWarningAndOwnerActivityAbort(t *testing.T) {
	s, ctx := testStore(t)
	owner := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	heir := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	p := policy.Defaults(owner)
	p.Notify.Succession = true
	p.Succession = &policy.Succession{Heir: heir, AfterDays: 90}
	transfers := 0
	r, err := New(ctx, Config{Community: emptyCommunityReader{}, Store: s, Policy: func() policy.Policy { return p }, SetPolicy: func(next policy.Policy) error { p = next; return nil }, RelayURL: "wss://relay.example", OnTransfer: func(context.Context, string, string) error { transfers++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	base := int64(1000)
	if err := r.Heartbeat(ctx, owner, base); err != nil {
		t.Fatal(err)
	}
	if err := r.Tick(ctx, base+90*86400+29*86400); err != nil {
		t.Fatal(err)
	}
	if transfers != 0 {
		t.Fatal("transferred during warning period")
	}
	if err := r.Heartbeat(ctx, owner, base+90*86400+29*86400+1); err != nil {
		t.Fatal(err)
	}
	if err := r.Tick(ctx, base+90*86400+31*86400); err != nil {
		t.Fatal(err)
	}
	if transfers != 0 {
		t.Fatal("owner heartbeat did not abort succession")
	}
	// Let the dead-man clock expire on a second relay instance: ownership must
	// be changed through the policy callback, not merely reported as a hook.
	p.Succession = &policy.Succession{Heir: heir, AfterDays: 90}
	if err := r.Heartbeat(ctx, owner, base); err != nil {
		t.Fatal(err)
	}
	if err := r.Tick(ctx, base+90*86400); err != nil {
		t.Fatal(err)
	}
	if err := r.Tick(ctx, base+90*86400+30*86400+1); err != nil {
		t.Fatal(err)
	}
	if p.Owner != heir || p.Succession != nil {
		t.Fatalf("succession did not transfer ownership: %+v", p)
	}
}

package configport

import (
	"context"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestParsePlanOmittedAndEmptySections(t *testing.T) {
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	owner := strings.Repeat("a", 64)
	c, err := community.New(ctx, st, owner)
	if err != nil {
		t.Fatal(err)
	}
	cur := policy.Defaults(owner)
	svc := New(ConfigStore{Store: st, Community: c, Policy: func() policy.Policy { return cur }})
	doc, err := Parse([]byte(`{"format":"bind.ws/relay-config/2","policy":{"reads":"members"}}`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := svc.Plan(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Policy) == 0 {
		t.Fatal("expected policy diff")
	}
	if len(plan.Members) != 0 {
		t.Fatal("omitted members changed plan")
	}
	doc, err = Parse([]byte(`{"format":"bind.ws/relay-config/2","members":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err = svc.Plan(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.MembersCleared {
		t.Fatal("empty members must clear")
	}
}

func TestApplyDryRunAndEconomyMigrationWarning(t *testing.T) {
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	owner := strings.Repeat("a", 64)
	_, err = community.New(ctx, st, owner)
	if err != nil {
		t.Fatal(err)
	}
	cur := policy.Defaults(owner)
	changed := false
	svc := New(ConfigStore{Store: st, Policy: func() policy.Policy { return cur }, OnApplied: func(policy.Policy) { changed = true }})
	doc, err := Parse([]byte(`{"format":"bind.ws/relay-config/2","policy":{"fuel":10,"reads":"auth"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Warnings) == 0 {
		t.Fatal("expected removed economy warning")
	}
	if _, err := svc.Apply(ctx, doc, true); err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("dry run mutated policy")
	}
}

func TestApplyAtomicAcrossPolicyMetadataAndJobs(t *testing.T) {
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	owner := strings.Repeat("a", 64)
	_, err = community.New(ctx, st, owner)
	if err != nil {
		t.Fatal(err)
	}
	cur := policy.Defaults(owner)
	if err := st.PutSetting(ctx, "policy", cur); err != nil {
		t.Fatal(err)
	}
	svc := New(ConfigStore{Store: st, Policy: func() policy.Policy { return cur }})
	if _, err := st.DB().Exec(`INSERT INTO config_jobs(id,recipe) VALUES('old','{"source":"old"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO community_members(pubkey,name,note,role,created_at) VALUES(?,?,?,?,?)`, strings.Repeat("b", 64), "old", "", "member", 1); err != nil {
		t.Fatal(err)
	}
	doc, err := Parse([]byte(`{"format":"bind.ws/relay-config/2","policy":{"reads":"auth"},"members":[{"pubkey":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","role":"member"}],"kinds":{"allow":[1],"block":[1]},"jobs":[{"id":"new","source":"new"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Apply(ctx, doc, false); err == nil {
		t.Fatal("expected duplicate kind rule failure")
	}
	var got policy.Policy
	if err := st.GetSetting(ctx, "policy", &got); err != nil {
		t.Fatal(err)
	}
	if got.Reads != cur.Reads {
		t.Fatalf("policy committed despite rollback: %q", got.Reads)
	}
	var jobs, members int
	if err := st.DB().QueryRow(`SELECT count(*) FROM config_jobs WHERE id='old'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT count(*) FROM community_members WHERE pubkey=?`, strings.Repeat("b", 64)).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 || members != 1 {
		t.Fatalf("metadata committed despite rollback: jobs=%d members=%d", jobs, members)
	}
}

func TestApplyJobsFeedsReplicationQueue(t *testing.T) {
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	owner := strings.Repeat("a", 64)
	if _, err := community.New(ctx, st, owner); err != nil {
		t.Fatal(err)
	}
	cur := policy.Defaults(owner)
	svc := New(ConfigStore{Store: st, Community: nil, Policy: func() policy.Policy { return cur }})
	doc, err := Parse([]byte(`{"format":"bind.ws/relay-config/2","jobs":[{"id":"pull-1","kind":"pull","relays":["wss://relay.example"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Apply(ctx, doc, false); err != nil {
		t.Fatal(err)
	}
	var payload string
	if err := st.DB().QueryRow(`SELECT payload FROM replication_jobs WHERE id='pull-1'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"kind":"pull"`) {
		t.Fatalf("replication payload: %s", payload)
	}
}

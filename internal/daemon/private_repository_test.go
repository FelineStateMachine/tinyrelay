package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/configport"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestPrivateRepositoryRejectedOnOpenTenantPublishAndImport(t *testing.T) {
	_, tenant := testTenant(t)
	p := tenant.Policy()
	p.Features.Grasp = true
	p.Reads = "open"
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("4", 64)
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	e := event.Event{Kind: event.KIND_REPO, PubKey: owner, CreatedAt: 1, Tags: [][]string{{"d", "private"}, {"private", "true"}, {"clone", "https://relay.example/private.git"}}}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.router.Publish(context.Background(), e, relay.Session{PubKeys: []string{owner}}); err == nil || !strings.Contains(err.Error(), "GRASP-08") {
		t.Fatalf("open publish error = %v", err)
	}
	if err := tenant.ingest(context.Background(), e, replication.OriginImport); err == nil || !strings.Contains(err.Error(), "GRASP-08") {
		t.Fatalf("open import error = %v", err)
	}
}

func TestPrivateRepositoryPolicyCannotBeReopened(t *testing.T) {
	owner, err := event.PublicKey(strings.Repeat("5", 63) + "1")
	if err != nil {
		t.Fatal(err)
	}
	_, tenant, _ := privatePeerTenant(t, "private-policy", owner)
	e := event.Event{Kind: event.KIND_REPO, PubKey: owner, CreatedAt: 1, Tags: [][]string{{"d", "private"}, {"private", "true"}}}
	if err := event.Sign(&e, strings.Repeat("5", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.Save(context.Background(), e, storage.SaveOptions{Now: 1}); err != nil {
		t.Fatal(err)
	}
	p := tenant.Policy()
	p.Features.Grasp08 = false
	p.Reads = "open"
	if err := tenant.applyPolicy(context.Background(), p); err == nil {
		t.Fatal("private policy transition was accepted")
	}
	patch := configport.Config{Format: configport.Format, Policy: map[string]json.RawMessage{
		"features": json.RawMessage(`{"grasp08":false}`), "reads": json.RawMessage(`"open"`),
	}}
	if _, err := tenant.config.Apply(context.Background(), patch, false); err == nil {
		t.Fatal("config import reopened a private tenant")
	}
	var stored policy.Policy
	if err := tenant.store.GetSetting(context.Background(), "policy", &stored); err != nil {
		t.Fatal(err)
	}
	if !tenant.PrivateServiceEnabled() || !stored.Features.Grasp08 || stored.Reads != "members" {
		t.Fatal("rejected import changed the effective or durable private policy")
	}
}

func TestLegacyPrivateRepositoryForcesBoundaryAfterRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	app, err := New(ctx, Config{DataDir: dir, DefaultTenant: "main"})
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("6", 63) + "1"
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := app.Create(ctx, CreateOptions{Name: "main", Owner: owner, Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := app.tenant(ctx, meta, "http://relay.test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	items := []event.Event{
		{Kind: event.KIND_REPO, PubKey: owner, CreatedAt: now, Tags: [][]string{{"d", "legacy"}, {"private", "true"}}},
		{Kind: event.KIND_REPO_STATE, PubKey: owner, CreatedAt: now + 1, Tags: [][]string{{"d", "legacy"}}},
		{Kind: 1621, PubKey: owner, CreatedAt: now + 2, Tags: [][]string{{"e", "legacy"}}},
	}
	for i := range items {
		if err := event.Sign(&items[i], secret); err != nil {
			t.Fatal(err)
		}
		if _, err := tenant.store.Save(ctx, items[i], storage.SaveOptions{Now: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Close(ctx); err != nil {
		t.Fatal(err)
	}
	app, err = New(ctx, Config{DataDir: dir, DefaultTenant: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(ctx, "http://relay.test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close(ctx) })
	reloaded := app.tenants[meta.ID]
	if reloaded == nil || !reloaded.PrivateServiceEnabled() || reloaded.Policy().Reads != "members" {
		t.Fatal("legacy private data did not force members boundary")
	}
	if _, err := reloaded.Query(ctx, []event.Filter{{Kinds: []int{event.KIND_REPO, event.KIND_REPO_STATE, 1621}}}, relay.Session{}); err == nil {
		t.Fatal("anonymous query reached legacy private metadata")
	}
	if _, err := reloaded.Count(ctx, []event.Filter{{Kinds: []int{event.KIND_REPO}}}, relay.Session{}); err == nil {
		t.Fatal("anonymous count reached legacy private metadata")
	}
	var durable policy.Policy
	if err := reloaded.store.GetSetting(ctx, "policy", &durable); err != nil {
		t.Fatal(err)
	}
	if !durable.Features.Grasp08 || durable.Reads != "members" {
		t.Fatal("legacy boundary must remain private after its announcement is removed")
	}
}

package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// TestCallbackIntentSurvivesRestartAndHonorsRevocation verifies the daemon
// lifecycle around callback work. The callback is queued transactionally,
// survives reopening the tenant, and is consumed without delivery once its
// registration is revoked before the recovered worker runs.
func TestCallbackIntentSurvivesRestartAndHonorsRevocation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	secret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{DataDir: dir, DefaultTenant: "main", PushCallbackOrigins: []string{"https://push.example"}}
	app, err := New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := app.Create(ctx, CreateOptions{Name: "main", Owner: owner, Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(ctx, "http://relay.test"); err != nil {
		t.Fatal(err)
	}
	tenant := app.tenants[meta.ID]
	if tenant == nil {
		t.Fatal("tenant was not started")
	}
	// Fence the production worker so the intent remains unconsumed until the
	// second App has reopened the tenant.
	tenant.workCancel()
	tenant.workWG.Wait()
	next := tenant.Policy()
	next.Features.Push = true
	next.PushCallbacks = []string{"https://push.example"}
	if err := tenant.applyPolicy(ctx, next); err != nil {
		t.Fatal(err)
	}
	registration := event.Event{Kind: event.KIND_PUSH_REGISTRATION, CreatedAt: time.Now().Unix(), Tags: [][]string{
		{"d", "restart-callback"},
		{"relay", tenant.RelayURL()},
		{"callback", "https://push.example"},
		{"filter", `{"kinds":[1]}`},
	}}
	if err := event.Sign(&registration, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.Save(ctx, registration, storage.SaveOptions{Now: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	delivered := event.Event{Kind: 1, CreatedAt: time.Now().Unix(), Content: "callback restart"}
	if err := event.Sign(&delivered, secret); err != nil {
		t.Fatal(err)
	}
	intents, err := tenant.PrepareReplicationCallbacks(ctx, delivered)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 || intents[0].Kind != "callback" {
		t.Fatalf("callback intents = %#v", intents)
	}
	if _, err := tenant.store.Save(ctx, delivered, storage.SaveOptions{Now: time.Now().Unix(), Intents: intents}); err != nil {
		t.Fatal(err)
	}
	// Keep recovery deterministic: the new App must not attempt delivery in
	// the interval between starting its worker and the policy revocation.
	if _, err := tenant.store.DB().ExecContext(ctx, `UPDATE work_intents SET next_at=? WHERE kind='callback'`, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(ctx); err != nil {
		t.Fatal(err)
	}

	app, err = New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	if err := app.Start(ctx, "http://relay.test"); err != nil {
		t.Fatal(err)
	}
	reloaded := app.tenants[meta.ID]
	if reloaded == nil {
		t.Fatal("tenant was not recovered")
	}
	// Stop the automatically recovered worker before changing policy, then
	// revoke the origin and let a fresh worker consume the recovered intent.
	reloaded.workCancel()
	reloaded.workWG.Wait()
	revoked := reloaded.Policy()
	revoked.Features.Push = true
	revoked.PushCallbacks = nil
	if err := reloaded.applyPolicy(ctx, revoked); err != nil {
		t.Fatal(err)
	}
	pending, err := reloaded.replication.Queue().Pending(ctx, "callback")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("recovered callback intents = %d", len(pending))
	}
	if err := reloaded.replication.Queue().RetryNow(ctx, pending[0].ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- reloaded.runWork(runCtx) }()
	if !waitFor(t, 3*time.Second, func() bool {
		items, listErr := reloaded.replication.Queue().Pending(ctx, "callback")
		return listErr == nil && len(items) == 0
	}) {
		t.Fatalf("revoked callback intent remained pending: %#v", pending)
	}
	cancel()
	if err := <-done; err != nil && !strings.Contains(err.Error(), "canceled") {
		t.Fatal(err)
	}
}

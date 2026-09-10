package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/work"
)

func TestCustomViewTargetsRejectRecreatedRegistration(t *testing.T) {
	tenant, server, _ := viewTenant(t, []int{1}, nil)
	ctx := context.Background()
	note := signedEvent(t, testOwnerSecret, 1, time.Now().Unix(), nil, "```mermaid\ngraph TD; a-->b;\n```")
	if err := publishAs(t, tenant, note); err != nil {
		t.Fatal(err)
	}
	targets, err := tenant.customViews.CandidateTargets(ctx, note)
	if err != nil || len(targets) != 1 {
		t.Fatalf("candidate targets = %+v, %v", targets, err)
	}
	oldIntents, err := tenant.customViews.PrepareTargets(ctx, note, targets, false)
	if err != nil || len(oldIntents) != 1 {
		t.Fatalf("planned intents = %+v, %v", oldIntents, err)
	}
	if _, err := tenant.Execute(ctx, tenant.Policy().Owner, "removecustomview", viewParams(map[string]any{"name": "diagrams"})); err != nil {
		t.Fatal(err)
	}
	addView(t, tenant, map[string]any{
		"name":      "diagrams",
		"kinds":     []int{1},
		"transform": "https://render.example/tiny",
		"languages": []string{"mermaid"},
		"secret":    "same-secret-value-1234",
	})
	targets, err = tenant.customViews.CandidateTargets(ctx, note)
	if err != nil || len(targets) != 1 {
		t.Fatalf("replacement candidate targets = %+v, %v", targets, err)
	}
	if _, err := tenant.Execute(ctx, tenant.Policy().Owner, "removecustomview", viewParams(map[string]any{"name": "diagrams"})); err != nil {
		t.Fatal(err)
	}
	addView(t, tenant, map[string]any{
		"name":      "diagrams",
		"kinds":     []int{1},
		"transform": "https://render.example/tiny",
		"languages": []string{"mermaid"},
		"secret":    "same-secret-value-1234",
	})
	intents, err := tenant.customViews.PrepareTargets(ctx, note, targets, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 0 {
		t.Fatalf("recreated view received old targets: %+v", intents)
	}
	if err := tenant.customViews.handleViewTransform(ctx, work.Intent{ID: "stale", Kind: oldIntents[0].Kind, EventID: oldIntents[0].EventID, Target: oldIntents[0].Target, Payload: oldIntents[0].Payload}); err != nil {
		t.Fatal(err)
	}
	if server.count() != 0 {
		t.Fatalf("stale generation contacted transform: %d requests", server.count())
	}
}

func TestCustomViewGenerationBackfillIsStable(t *testing.T) {
	tenant, _, _ := viewTenant(t, []int{1}, nil)
	ctx := context.Background()
	if _, err := tenant.store.DB().ExecContext(ctx, `ALTER TABLE custom_views DROP COLUMN generation`); err != nil {
		t.Fatal(err)
	}
	if err := tenant.customViews.initCustomViews(ctx); err != nil {
		t.Fatal(err)
	}
	var first string
	if err := tenant.store.DB().QueryRowContext(ctx, `SELECT generation FROM custom_views WHERE name='diagrams'`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("legacy view generation was not backfilled")
	}
	if err := tenant.customViews.initCustomViews(ctx); err != nil {
		t.Fatal(err)
	}
	var second string
	if err := tenant.store.DB().QueryRowContext(ctx, `SELECT generation FROM custom_views WHERE name='diagrams'`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("generation changed on reopen: %q != %q", first, second)
	}
}

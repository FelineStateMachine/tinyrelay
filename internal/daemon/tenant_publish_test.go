package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
)

func TestPublishReportIsPrivateAndMetadataIsAtomic(t *testing.T) {
	ctx := context.Background()
	app, err := New(ctx, Config{DataDir: t.TempDir(), DefaultTenant: "main"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	secret := strings.Repeat("0", 63) + "1"
	owner, _ := event.PublicKey(secret)
	meta, err := app.Create(ctx, CreateOptions{Name: "main", Owner: owner, Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := app.tenant(ctx, meta, "http://relay.test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	report := event.Event{Kind: event.KIND_REPORT, CreatedAt: now, Tags: [][]string{{"e", strings.Repeat("a", 64), "spam"}}, Content: "bad"}
	if err := event.Sign(&report, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.Publish(ctx, report, relay.Session{PubKeys: []string{owner}}); err != nil {
		t.Fatal(err)
	}
	var reports, stored int
	if err := tenant.store.DB().QueryRow(`SELECT count(*) FROM community_reports`).Scan(&reports); err != nil {
		t.Fatal(err)
	}
	if err := tenant.store.DB().QueryRow(`SELECT count(*) FROM events WHERE id=?`, report.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if reports != 1 || stored != 0 {
		t.Fatalf("report reports=%d stored=%d", reports, stored)
	}
	metadata := event.Event{Kind: event.KIND_EDIT_METADATA, CreatedAt: now + 1, Tags: [][]string{{"name", "renamed"}}}
	if err := event.Sign(&metadata, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.Publish(ctx, metadata, relay.Session{PubKeys: []string{owner}}); err != nil {
		t.Fatal(err)
	}
	var current policy.Policy
	if err := tenant.store.GetSetting(ctx, "policy", &current); err != nil {
		t.Fatal(err)
	}
	if current.Name != "renamed" {
		t.Fatalf("policy name=%q", current.Name)
	}
}

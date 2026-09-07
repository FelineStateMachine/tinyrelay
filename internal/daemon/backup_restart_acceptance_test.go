package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
)

func TestDaemonBackupJobResumesAfterRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	secret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{DataDir: dir, DefaultTenant: "main"}
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
	tenant.workCancel()
	tenant.workWG.Wait()
	if err := tenant.replication.AddJob(ctx, replication.JobSpec{ID: "restart-backup", Kind: replication.JobBackup, Relays: []string{"restart.backup.json"}}); err != nil {
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
	if !waitFor(t, 8*time.Second, func() bool {
		statuses, statusErr := reloaded.replication.ListJobStatus(ctx)
		return statusErr == nil && len(statuses) == 1 && statuses[0].Phase == "complete"
	}) {
		statuses, _ := reloaded.replication.ListJobStatus(ctx)
		t.Fatalf("backup job did not resume: %#v", statuses)
	}
	if _, err := os.Stat(meta.Paths.Root + "/restart.backup.json"); err != nil {
		t.Fatalf("backup artifact missing after restart: %v", err)
	}
}

func TestDaemonFailedJobStatusSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	secret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{DataDir: dir, DefaultTenant: "main", AllowPrivateRelays: true}
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
	tenant.workCancel()
	tenant.workWG.Wait()
	if err := tenant.replication.AddJob(ctx, replication.JobSpec{ID: "restart-failure", Kind: replication.JobPull, Relays: []string{"ws://127.0.0.1:1"}}); err != nil {
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
	if !waitFor(t, 8*time.Second, func() bool {
		statuses, statusErr := reloaded.replication.ListJobStatus(ctx)
		return statusErr == nil && len(statuses) == 1 && statuses[0].Phase == "failed" && statuses[0].Error != ""
	}) {
		statuses, _ := reloaded.replication.ListJobStatus(ctx)
		t.Fatalf("failed job status was not recorded after restart: %#v", statuses)
	}
	pending, err := reloaded.replication.Queue().Pending(ctx, "job")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Attempts == 0 {
		t.Fatalf("failed job lost retry state: %#v", pending)
	}
}

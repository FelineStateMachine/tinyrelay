package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestGitPromotionRollsBackVisibilityOnStorageFailure(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	e := event.Event{Kind: 30618, CreatedAt: time.Now().Unix(), Tags: [][]string{{"d", "repo"}}, Content: ""}
	if err := event.Sign(&e, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.Save(ctx, e, storage.SaveOptions{Now: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS replication_pending_until(id TEXT PRIMARY KEY,until INTEGER NOT NULL)`,
		`INSERT INTO pending_events(id,reason) VALUES('` + e.ID + `','git objects')`,
		`INSERT INTO replication_pending_until(id,until) VALUES('` + e.ID + `',1)`,
		`CREATE TRIGGER fail_promotion BEFORE DELETE ON replication_pending_until BEGIN SELECT RAISE(ABORT,'injected storage failure'); END`,
	} {
		if _, err := tenant.store.DB().ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := tenant.releaseGit(ctx, e.ID); err == nil {
		t.Fatal("promotion ignored storage failure")
	}
	var held int
	if err := tenant.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM pending_events WHERE id=?`, e.ID).Scan(&held); err != nil || held != 1 {
		t.Fatalf("failed promotion lost retry marker: %d, %v", held, err)
	}
	if _, err := tenant.store.DB().ExecContext(ctx, `DROP TRIGGER fail_promotion`); err != nil {
		t.Fatal(err)
	}
	if err := tenant.releaseGit(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	result, err := tenant.store.Query(ctx, event.Filter{IDs: []string{e.ID}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}})
	if err != nil || len(result.Events) != 1 {
		t.Fatalf("retried promotion not visible: %#v, %v", result, err)
	}
}

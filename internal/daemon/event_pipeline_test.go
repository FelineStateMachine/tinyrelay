package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/work"
)

func TestOptionalFollowupFailurePreservesEventAndRecovery(t *testing.T) {
	for _, source := range []string{"client", "import"} {
		t.Run(source, func(t *testing.T) {
			tenant, remote, _ := viewTenant(t, []int{30023}, nil)
			ctx := context.Background()
			e := signedEvent(t, testOwnerSecret, 30023, time.Now().Unix(), [][]string{{"d", "recover-view"}}, "```mermaid\ngraph LR; A-->B\n```")
			if _, err := tenant.store.DB().ExecContext(ctx, `CREATE TRIGGER reject_view_work BEFORE INSERT ON work_intents WHEN NEW.kind='view-transform' BEGIN SELECT RAISE(ABORT,'view work unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			if source == "import" {
				if err := tenant.commitImported(ctx, e, replication.OriginImport); err != nil {
					t.Fatal(err)
				}
			} else if _, err := tenant.Publish(ctx, e, relay.Session{PubKeys: []string{e.PubKey}}); err != nil {
				t.Fatal(err)
			}
			var stored int
			if err := tenant.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE id=?`, e.ID).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if stored != 1 {
				t.Fatal("optional planning failure rejected valid event")
			}
			job := followupJob(t, tenant, e.ID)
			if remote.count() != 0 {
				t.Fatal("planning contacted the remote transform")
			}
			if _, err := tenant.store.DB().ExecContext(ctx, `DROP TRIGGER reject_view_work`); err != nil {
				t.Fatal(err)
			}
			intent := storage.Intent{Kind: job.Kind, EventID: job.EventID, Target: job.Target, Payload: job.Payload}
			tenant.followups.try(ctx, &intent)
			if intents := pendingViewIntents(t, tenant, "diagrams", e.ID); len(intents) != 1 {
				t.Fatalf("recovery queued %d view intents, want one", len(intents))
			}
			var state string
			if err := tenant.store.DB().QueryRowContext(ctx, `SELECT state FROM work_intents WHERE id=?`, job.ID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != "completed" {
				t.Fatalf("recovered planning state=%s", state)
			}
			if _, err := tenant.store.DB().ExecContext(ctx, `UPDATE work_intents SET state='completed' WHERE kind=? AND event_id=?`, viewTransform, e.ID); err != nil {
				t.Fatal(err)
			}
			if err := tenant.followups.handle(ctx, job); err != nil {
				t.Fatal(err)
			}
			if intents := pendingViewIntents(t, tenant, "diagrams", e.ID); len(intents) != 0 {
				t.Fatal("recovery replayed already completed transform work")
			}
		})
	}
}

func TestFollowupPlanningSkipsRemovedSourcesAndNewTargets(t *testing.T) {
	tenant, _, _ := viewTenant(t, []int{30023}, nil)
	ctx := context.Background()
	e := signedEvent(t, testOwnerSecret, 30023, time.Now().Unix(), [][]string{{"d", "captured-targets"}}, "```mermaid\ngraph LR; A-->B\n```")
	intent := tenant.followups.prepare(ctx, e, time.Now().Unix(), false)
	if intent == nil {
		t.Fatal("matching view did not create planning intent")
	}
	if _, err := tenant.store.Save(ctx, e, storage.SaveOptions{Now: time.Now().Unix(), Intents: []storage.Intent{*intent}}); err != nil {
		t.Fatal(err)
	}
	addView(t, tenant, map[string]any{"name": "later", "kinds": []int{30023}, "transform": "https://later.example/render", "languages": []string{"mermaid"}})
	job := followupJob(t, tenant, e.ID)
	if err := tenant.followups.handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	if intents := pendingViewIntents(t, tenant, "later", e.ID); len(intents) != 0 {
		t.Fatal("new view received an older captured event")
	}
	if _, err := tenant.store.DB().ExecContext(ctx, `DELETE FROM work_intents WHERE kind=? AND event_id=?`, viewTransform, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.DB().ExecContext(ctx, `DELETE FROM events WHERE id=?`, e.ID); err != nil {
		t.Fatal(err)
	}
	if err := tenant.followups.handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	if intents := pendingViewIntents(t, tenant, "diagrams", e.ID); len(intents) != 0 {
		t.Fatal("removed source was replanned")
	}
	var payload eventFollowupPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	payload.AcceptedAt = time.Now().Add(-followupMaxAge - time.Minute).Unix()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	job.Payload = string(raw)
	if err := tenant.followups.handle(ctx, job); err == nil {
		t.Fatal("expired optional planning was not stopped")
	}
}

func followupJob(t *testing.T, tenant *Tenant, eventID string) work.Intent {
	t.Helper()
	job := work.Intent{Kind: eventFollowupKind, EventID: eventID, Attempts: 1}
	if err := tenant.store.DB().QueryRow(`SELECT id,target,payload FROM work_intents WHERE kind=? AND event_id=?`, eventFollowupKind, eventID).Scan(&job.ID, &job.Target, &job.Payload); err != nil {
		t.Fatal(err)
	}
	return job
}

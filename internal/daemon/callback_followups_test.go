package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestCallbackFollowupPlanningFailureDoesNotRejectEvents(t *testing.T) {
	for _, source := range []string{"client", "import"} {
		t.Run(source, func(t *testing.T) {
			_, tenant := loopbackTenant(t)
			ctx := context.Background()
			member, _ := event.PublicKey(testMemberSecret)
			setRole(t, tenant, member, "member")
			addCallback(t, tenant, member, "https://127.0.0.1:1/wake", `{"kinds":[1]}`)
			if _, err := tenant.store.DB().ExecContext(ctx, `CREATE TRIGGER reject_callback_work BEFORE INSERT ON work_intents WHEN NEW.kind='callback-delivery' BEGIN SELECT RAISE(ABORT,'callback work unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			e := signedEvent(t, testOwnerSecret, 1, time.Now().Unix(), [][]string{{"p", member}}, "callback planning")
			var err error
			if source == "import" {
				err = tenant.commitImported(ctx, e, replication.OriginImport)
			} else {
				_, err = tenant.Publish(ctx, e, relay.Session{PubKeys: []string{e.PubKey}})
			}
			if err != nil {
				t.Fatalf("%s event rejected after optional planning failure: %v", source, err)
			}
			var stored int
			if err := tenant.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE id=?`, e.ID).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if stored != 1 {
				t.Fatalf("stored events=%d, want 1", stored)
			}
			var pending int
			if err := tenant.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM work_intents WHERE kind=? AND event_id=? AND state='pending'`, eventFollowupKind, e.ID).Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if pending != 1 {
				t.Fatalf("pending follow-up jobs=%d, want 1", pending)
			}
		})
	}
}

func TestCallbackFollowupRecoveryPreservesCompletedDelivery(t *testing.T) {
	_, tenant := loopbackTenant(t)
	ctx := context.Background()
	member, _ := event.PublicKey(testMemberSecret)
	setRole(t, tenant, member, "member")
	first := addCallback(t, tenant, member, "https://127.0.0.1:1/first", `{"kinds":[1]}`)
	second := addCallback(t, tenant, member, "https://127.0.0.1:1/second", `{"kinds":[1]}`)
	e := signedEvent(t, testOwnerSecret, 1, time.Now().Unix(), [][]string{{"p", member}}, "recover callback work")
	followup := tenant.followups.prepare(ctx, e, time.Now().Unix(), false)
	if followup == nil {
		t.Fatal("missing callback follow-up")
	}
	if _, err := tenant.store.Save(ctx, e, storage.SaveOptions{Now: time.Now().Unix(), Intents: []storage.Intent{*followup}}); err != nil {
		t.Fatal(err)
	}
	job := followupJob(t, tenant, e.ID)
	if err := tenant.followups.handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	firstID := first["id"].(string)
	secondID := second["id"].(string)
	markCallbackDeliveryCompleted(t, tenant, firstID, e.ID)
	deleteCallbackDelivery(t, tenant, secondID, e.ID)
	if err := tenant.followups.handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	if state := callbackDeliveryState(t, tenant, firstID, e.ID); state != "completed" {
		t.Fatalf("completed delivery state=%q, want completed", state)
	}
	if state := callbackDeliveryState(t, tenant, secondID, e.ID); state != "pending" {
		t.Fatalf("recovered delivery state=%q, want pending", state)
	}
}

func TestCallbackFollowupSkipsPausedRemovedAndLaterRegistrations(t *testing.T) {
	for _, action := range []string{"pausecallback", "removecallback"} {
		t.Run(action, func(t *testing.T) {
			_, tenant := loopbackTenant(t)
			ctx := context.Background()
			member, _ := event.PublicKey(testMemberSecret)
			setRole(t, tenant, member, "member")
			captured := addCallback(t, tenant, member, "https://127.0.0.1:1/captured", `{"kinds":[1]}`)
			e := signedEvent(t, testOwnerSecret, 1, time.Now().Unix(), [][]string{{"p", member}}, "captured callback")
			followup := tenant.followups.prepare(ctx, e, time.Now().Unix(), false)
			if followup == nil {
				t.Fatal("missing callback follow-up")
			}
			id := captured["id"].(string)
			if _, err := tenant.Execute(ctx, member, action, []json.RawMessage{json.RawMessage(strconv.Quote(id))}); err != nil {
				t.Fatal(err)
			}
			later := addCallback(t, tenant, member, "https://127.0.0.1:1/later", `{"kinds":[1]}`)
			if _, err := tenant.store.Save(ctx, e, storage.SaveOptions{Now: time.Now().Unix(), Intents: []storage.Intent{*followup}}); err != nil {
				t.Fatal(err)
			}
			if err := tenant.followups.handle(ctx, followupJob(t, tenant, e.ID)); err != nil {
				t.Fatal(err)
			}
			if got := callbackDeliveryState(t, tenant, id, e.ID); got != "" {
				t.Fatalf("%s callback state=%q, want absent", action, got)
			}
			if got := callbackDeliveryState(t, tenant, later["id"].(string), e.ID); got != "" {
				t.Fatalf("later callback state=%q, want absent", got)
			}
		})
	}
}

func TestCallbackFollowupPlanningExhaustionStops(t *testing.T) {
	_, tenant := loopbackTenant(t)
	ctx := context.Background()
	member, _ := event.PublicKey(testMemberSecret)
	setRole(t, tenant, member, "member")
	addCallback(t, tenant, member, "https://127.0.0.1:1/wake", `{"kinds":[1]}`)
	e := signedEvent(t, testOwnerSecret, 1, time.Now().Unix(), [][]string{{"p", member}}, "exhaust planning")
	followup := tenant.followups.prepare(ctx, e, time.Now().Unix(), false)
	if followup == nil {
		t.Fatal("missing callback follow-up")
	}
	if _, err := tenant.store.Save(ctx, e, storage.SaveOptions{Now: time.Now().Unix(), Intents: []storage.Intent{*followup}}); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.DB().ExecContext(ctx, `CREATE TRIGGER reject_callback_work BEFORE INSERT ON work_intents WHEN NEW.kind='callback-delivery' BEGIN SELECT RAISE(ABORT,'callback work unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	job := followupJob(t, tenant, e.ID)
	job.Attempts = followupMaxAttempts
	err := tenant.followups.handle(ctx, job)
	if err == nil || !strings.Contains(err.Error(), "callback work unavailable") || errors.Unwrap(err) == nil {
		t.Fatalf("exhausted planning error=%v, want terminal wrapped error", err)
	}
}

func TestCallbackFollowupRecoversWhenViewLookupIsUnavailable(t *testing.T) {
	tenant, _, _ := viewTenant(t, []int{1}, nil)
	ctx := context.Background()
	member, _ := event.PublicKey(testMemberSecret)
	setRole(t, tenant, member, "member")
	callback := addCallback(t, tenant, member, "https://127.0.0.1:1/wake", `{"kinds":[1]}`)
	e := signedEvent(t, testOwnerSecret, 1, time.Now().Unix(), [][]string{{"p", member}}, "```mermaid\ngraph LR; A-->B\n```")
	if _, err := tenant.store.DB().ExecContext(ctx, `ALTER TABLE custom_views RENAME TO custom_views_unavailable`); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.Publish(ctx, e, relay.Session{PubKeys: []string{e.PubKey}}); err != nil {
		t.Fatalf("publish with unavailable optional view lookup: %v", err)
	}
	job := followupJob(t, tenant, e.ID)
	callbackID := callback["id"].(string)
	if state := callbackDeliveryState(t, tenant, callbackID, e.ID); state != "pending" {
		t.Fatalf("callback state after partial planning=%q, want pending", state)
	}
	markCallbackDeliveryCompleted(t, tenant, callbackID, e.ID)
	if _, err := tenant.store.DB().ExecContext(ctx, `ALTER TABLE custom_views_unavailable RENAME TO custom_views`); err != nil {
		t.Fatal(err)
	}
	if err := tenant.followups.handle(ctx, job); err != nil {
		t.Fatalf("recover follow-up planning: %v", err)
	}
	if state := callbackDeliveryState(t, tenant, callbackID, e.ID); state != "completed" {
		t.Fatalf("callback state after recovery=%q, want completed", state)
	}
	var callbackCount int
	if err := tenant.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM work_intents WHERE kind=? AND target=? AND event_id=?`, callbackDelivery, callbackID, e.ID).Scan(&callbackCount); err != nil {
		t.Fatal(err)
	}
	if callbackCount != 1 {
		t.Fatalf("callback intent count=%d, want 1", callbackCount)
	}
}

func callbackDeliveryState(t *testing.T, tenant *Tenant, target, eventID string) string {
	t.Helper()
	var state string
	err := tenant.store.DB().QueryRow(`SELECT state FROM work_intents WHERE kind=? AND target=? AND event_id=?`, callbackDelivery, target, eventID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func markCallbackDeliveryCompleted(t *testing.T, tenant *Tenant, target, eventID string) {
	t.Helper()
	if _, err := tenant.store.DB().Exec(`UPDATE work_intents SET state='completed' WHERE kind=? AND target=? AND event_id=?`, callbackDelivery, target, eventID); err != nil {
		t.Fatal(err)
	}
}

func deleteCallbackDelivery(t *testing.T, tenant *Tenant, target, eventID string) {
	t.Helper()
	if _, err := tenant.store.DB().Exec(`DELETE FROM work_intents WHERE kind=? AND target=? AND event_id=?`, callbackDelivery, target, eventID); err != nil {
		t.Fatal(err)
	}
}

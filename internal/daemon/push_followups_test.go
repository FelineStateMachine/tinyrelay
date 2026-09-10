package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func enablePushPlanning(t *testing.T, tenant *Tenant) {
	t.Helper()
	tenant.app.cfg.PushCallbackOrigins = []string{"https://push.example"}
	p := tenant.Policy()
	p.Features.Push = true
	p.PushCallbacks = []string{"https://push.example"}
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func makeReplicationPushRegistration(t *testing.T, tenant *Tenant, d string, created int64, kinds ...int) event.Event {
	t.Helper()
	if len(kinds) == 0 {
		kinds = []int{1}
	}
	filter := `{"kinds":[1]}`
	if kinds[0] != 1 {
		filter = `{"kinds":[30618]}`
	}
	registration := event.Event{Kind: event.KIND_PUSH_REGISTRATION, CreatedAt: created, Tags: [][]string{
		{"d", d}, {"relay", tenant.RelayURL()}, {"callback", "https://push.example"}, {"filter", filter},
	}}
	if err := event.Sign(&registration, testOwnerSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.Save(context.Background(), registration, storage.SaveOptions{Now: created}); err != nil {
		t.Fatal(err)
	}
	return registration
}

func TestReplicationPushFollowupUsesInsertionCutoff(t *testing.T) {
	_, tenant := loopbackTenant(t)
	enablePushPlanning(t, tenant)
	ctx := context.Background()
	first := makeReplicationPushRegistration(t, tenant, "first", 100)
	e := signedEvent(t, testOwnerSecret, 1, 101, nil, "push cutoff")
	payload, err := tenant.prepareReplicationPushFollowup(ctx, e, 102)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.RegistrationIDs) != 1 || payload.RegistrationIDs[0] != first.ID {
		t.Fatalf("captured registrations = %#v", payload.RegistrationIDs)
	}
	if payload.RegistrationCutoff == 0 {
		t.Fatal("missing insertion cutoff")
	}
	if _, err := tenant.store.Save(ctx, e, storage.SaveOptions{Now: 102}); err != nil {
		t.Fatal(err)
	}
	makeReplicationPushRegistration(t, tenant, "later", 1)
	payload.Discover = true
	if err := tenant.recoverReplicationPushFollowup(ctx, &payload); err != nil {
		t.Fatal(err)
	}
	intents, err := tenant.prepareReplicationPushIntents(ctx, payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 || intents[0].Target != first.ID {
		t.Fatalf("recovered callback intents = %#v", intents)
	}
}

func TestReplicationPushFollowupSkipsSupersededRegistration(t *testing.T) {
	_, tenant := loopbackTenant(t)
	enablePushPlanning(t, tenant)
	ctx := context.Background()
	old := makeReplicationPushRegistration(t, tenant, "same", 100)
	e := signedEvent(t, testOwnerSecret, 1, 101, nil, "push replacement")
	payload, err := tenant.prepareReplicationPushFollowup(ctx, e, 102)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.Save(ctx, e, storage.SaveOptions{Now: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	newer := event.Event{Kind: event.KIND_PUSH_REGISTRATION, CreatedAt: 200, Tags: [][]string{
		{"d", "same"}, {"relay", tenant.RelayURL()}, {"callback", "https://push.example"}, {"filter", `{"kinds":[1]}`},
	}}
	if err := event.Sign(&newer, testOwnerSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.Save(ctx, newer, storage.SaveOptions{Now: 200}); err != nil {
		t.Fatal(err)
	}
	intents, err := tenant.prepareReplicationPushIntents(ctx, payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 0 {
		t.Fatalf("superseded callback intents = %#v (old registration %s)", intents, old.ID)
	}
}

func TestReplicationPushFollowupFailureKeepsClientEventDurable(t *testing.T) {
	_, tenant := loopbackTenant(t)
	enablePushPlanning(t, tenant)
	ctx := context.Background()
	registration := makeReplicationPushRegistration(t, tenant, "durable", 100)
	if _, err := tenant.store.DB().ExecContext(ctx, `CREATE TRIGGER reject_replication_push_work BEFORE INSERT ON work_intents WHEN NEW.kind='callback' BEGIN SELECT RAISE(ABORT,'push work unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	e := signedEvent(t, testOwnerSecret, 1, 101, nil, "durable push")
	if _, err := tenant.Publish(ctx, e, relay.Session{PubKeys: []string{e.PubKey}}); err != nil {
		t.Fatalf("publish rejected after optional push planning failure: %v", err)
	}
	var stored int
	if err := tenant.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE id=?`, e.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("stored events=%d, want 1", stored)
	}
	job := followupJob(t, tenant, e.ID)
	if job.Kind != eventFollowupKind {
		t.Fatalf("follow-up kind=%q", job.Kind)
	}
	if err := tenant.store.DB().QueryRowContext(ctx, `SELECT state FROM work_intents WHERE id=?`, job.ID).Scan(&job.State); err != nil {
		t.Fatal(err)
	}
	if job.State != "pending" {
		t.Fatalf("parent state=%q, want pending", job.State)
	}
	if _, err := tenant.store.DB().ExecContext(ctx, `DROP TRIGGER reject_replication_push_work`); err != nil {
		t.Fatal(err)
	}
	parent := storage.Intent{Kind: job.Kind, EventID: job.EventID, Target: job.Target, Payload: job.Payload}
	tenant.followups.try(ctx, &parent)
	var parentState string
	if err := tenant.store.DB().QueryRowContext(ctx, `SELECT state FROM work_intents WHERE id=?`, job.ID).Scan(&parentState); err != nil {
		t.Fatal(err)
	}
	if parentState != "completed" {
		t.Fatalf("recovered parent state=%q, want completed", parentState)
	}
	var callbackState string
	if err := tenant.store.DB().QueryRowContext(ctx, `SELECT state FROM work_intents WHERE kind='callback' AND event_id=? AND target=?`, e.ID, registration.ID).Scan(&callbackState); err != nil {
		t.Fatal(err)
	}
	if callbackState != "pending" {
		t.Fatalf("derived callback state=%q, want pending", callbackState)
	}
	if _, err := tenant.store.DB().ExecContext(ctx, `UPDATE work_intents SET state='completed' WHERE kind='callback' AND event_id=? AND target=?`, e.ID, registration.ID); err != nil {
		t.Fatal(err)
	}
	tenant.followups.try(ctx, &parent)
	if err := tenant.store.DB().QueryRowContext(ctx, `SELECT state FROM work_intents WHERE kind='callback' AND event_id=? AND target=?`, e.ID, registration.ID).Scan(&callbackState); err != nil {
		t.Fatal(err)
	}
	if callbackState != "completed" {
		t.Fatalf("recovered completed callback state=%q, want completed", callbackState)
	}
}

func TestReplicationPushFollowupSkipsRevokedPolicy(t *testing.T) {
	_, tenant := loopbackTenant(t)
	enablePushPlanning(t, tenant)
	ctx := context.Background()
	registration := makeReplicationPushRegistration(t, tenant, "revoked", 100)
	e := signedEvent(t, testOwnerSecret, 1, 101, nil, "revoked push")
	payload, err := tenant.prepareReplicationPushFollowup(ctx, e, 102)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.Save(ctx, e, storage.SaveOptions{Now: 102}); err != nil {
		t.Fatal(err)
	}
	payload.RegistrationIDs = []string{registration.ID}
	p := tenant.Policy()
	p.Features.Push = false
	p.PushCallbacks = nil
	if err := tenant.applyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	intents, err := tenant.planReplicationPushFollowup(ctx, payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 0 {
		t.Fatalf("revoked push intents=%d, want 0", len(intents))
	}
}

func TestReplicationPushFollowupGitReleaseUsesCapturedRegistrations(t *testing.T) {
	// A pending source retains its client snapshot in the parent follow-up;
	// releasedPush must recover that snapshot instead of rediscovering later
	// registrations.
	_, tenant := loopbackTenant(t)
	enablePushPlanning(t, tenant)
	ctx := context.Background()
	first := makeReplicationPushRegistration(t, tenant, "git-first", 100, event.KIND_REPO_STATE)
	e := signedEvent(t, testOwnerSecret, event.KIND_REPO_STATE, 101, nil, "pending git")
	payload, err := tenant.prepareReplicationPushFollowup(ctx, e, 102)
	if err != nil {
		t.Fatal(err)
	}
	followup := tenant.followups.prepareWithPush(ctx, e, 102, false, &payload)
	if followup == nil {
		t.Fatal("missing pending git follow-up")
	}
	if _, err := tenant.store.Save(ctx, e, storage.SaveOptions{Now: 102, Intents: []storage.Intent{*followup}}); err != nil {
		t.Fatal(err)
	}
	makeReplicationPushRegistration(t, tenant, "git-later", 1, event.KIND_REPO_STATE)
	released, err := tenant.followups.releasedPush(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if released == nil || len(released.RegistrationIDs) != 1 || released.RegistrationIDs[0] != first.ID {
		t.Fatalf("released push snapshot=%#v", released)
	}
	intents, err := tenant.prepareReplicationPushIntents(ctx, *released)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 || intents[0].Target != first.ID {
		t.Fatalf("released push intents=%#v", intents)
	}
}

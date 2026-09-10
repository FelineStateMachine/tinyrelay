package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/telemetry"
	"github.com/FelineStateMachine/tinyrelay/internal/work"
)

const (
	eventFollowupKind   = "event-followups"
	followupMaxAge      = 15 * time.Minute
	followupMaxAttempts = 3
)

// eventFollowups plans optional work without depending on a remote service.
// Its outbox record commits with the source event; delivery and transforms
// retain their own bounded retry policies and deterministic intent IDs.
type eventFollowups struct {
	store     *storage.Store
	callbacks *callbackService
	views     *customViewService
	telemetry *telemetry.Telemetry
	push      func(context.Context, replicationPushFollowupPayload) ([]storage.Intent, error)
}

type eventFollowupPayload struct {
	Event             event.Event                     `json:"event"`
	AcceptedAt        int64                           `json:"accepted_at"`
	Released          bool                            `json:"released,omitempty"`
	Callbacks         []string                        `json:"callbacks,omitempty"`
	Views             []customViewTarget              `json:"views,omitempty"`
	DiscoverCallbacks bool                            `json:"discover_callbacks,omitempty"`
	DiscoverViews     bool                            `json:"discover_views,omitempty"`
	Push              *replicationPushFollowupPayload `json:"push,omitempty"`
	RecoverPush       bool                            `json:"recover_push,omitempty"`
}

func (f *eventFollowups) prepare(ctx context.Context, e event.Event, now int64, released bool) *storage.Intent {
	return f.prepareWithPush(ctx, e, now, released, nil)
}

func (f *eventFollowups) prepareWithPush(ctx context.Context, e event.Event, now int64, released bool, push *replicationPushFollowupPayload) *storage.Intent {
	if event.IsEphemeral(e.Kind) {
		return nil
	}
	payload := eventFollowupPayload{Event: e, AcceptedAt: now, Released: released, Push: push}
	if released && push == nil {
		var err error
		payload.Push, err = f.releasedPush(ctx, e.ID)
		payload.RecoverPush = err != nil
	}
	var callbackErr, viewErr error
	if e.Kind != event.KIND_REPO_STATE || released {
		payload.Callbacks, callbackErr = f.callbacks.CandidateIDs(ctx, e)
		payload.Views, viewErr = f.views.CandidateTargets(ctx, e)
	}
	payload.DiscoverCallbacks = callbackErr != nil
	payload.DiscoverViews = viewErr != nil
	if len(payload.Callbacks) == 0 && len(payload.Views) == 0 && callbackErr == nil && viewErr == nil && payload.Push == nil && !payload.RecoverPush {
		return nil
	}
	// Event and target fields contain no custom JSON marshalers.
	raw, err := json.Marshal(payload)
	if err != nil {
		f.telemetry.Logger().Error("encode optional event follow-ups", "error", err)
		return nil
	}
	target := "stored"
	if released {
		target = "released"
	}
	return &storage.Intent{Kind: eventFollowupKind, EventID: e.ID, Target: target, Payload: string(raw)}
}

// try runs only local planning after commit. A failure leaves the outbox
// pending for the worker; success marks planning complete with its derived
// intents, so a later retry never replays already planned remote work.
func (f *eventFollowups) try(ctx context.Context, intent *storage.Intent) {
	if intent == nil {
		return
	}
	job := work.Intent{ID: storage.IntentID(intent.Kind, intent.EventID, intent.Target), Kind: intent.Kind, EventID: intent.EventID, Target: intent.Target, Payload: intent.Payload}
	if err := f.plan(ctx, job, true); err != nil {
		f.telemetry.Logger().Debug("optional event follow-ups deferred", "error", err)
	}
}

func (f *eventFollowups) handle(ctx context.Context, job work.Intent) error {
	err := f.plan(ctx, job, false)
	if err != nil && job.Attempts >= followupMaxAttempts {
		return work.Stop(fmt.Errorf("optional follow-up planning exhausted: %w", err))
	}
	return err
}

func (f *eventFollowups) plan(ctx context.Context, job work.Intent, pending bool) error {
	var payload eventFollowupPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil || payload.Event.ID != job.EventID {
		return work.Stop(errors.New("invalid optional event follow-up"))
	}
	if time.Now().Unix()-payload.AcceptedAt > int64(followupMaxAge/time.Second) {
		return work.Stop(errors.New("optional event follow-up expired"))
	}
	active, err := f.sourceActive(ctx, payload.Event)
	if err != nil {
		return err
	}
	var intents []storage.Intent
	var planningErr error
	if active {
		intents, planningErr = f.intents(ctx, payload)
	}
	err = f.store.WithTx(ctx, func(tx *sql.Tx) error {
		if err := storage.AddIntents(ctx, tx, intents, time.Now().Unix()); err != nil {
			return err
		}
		if pending && planningErr == nil {
			_, err := work.CompletePendingTx(ctx, tx, job.ID, time.Now())
			return err
		}
		return nil
	})
	return errors.Join(planningErr, err)
}

func (f *eventFollowups) sourceActive(ctx context.Context, e event.Event) (bool, error) {
	result, err := f.store.Query(ctx, event.Filter{IDs: []string{e.ID}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
	return len(result.Events) != 0, err
}

func (f *eventFollowups) intents(ctx context.Context, payload eventFollowupPayload) ([]storage.Intent, error) {
	callbacks, callbackErr := f.callbackIntents(ctx, payload)
	views, viewErr := f.viewIntents(ctx, payload)
	intents := append(callbacks, views...)
	var pushErr error
	if payload.RecoverPush {
		payload.Push, pushErr = f.releasedPush(ctx, payload.Event.ID)
	}
	if payload.Push != nil {
		push, err := f.push(ctx, *payload.Push)
		pushErr = err
		intents = append(intents, push...)
	}
	return intents, errors.Join(callbackErr, viewErr, pushErr)
}

// releasedPush preserves the registrations captured at client acceptance.
// Git promotion can happen later, after its initial planner has found the
// source still pending. Imports have no client push snapshot to recover.
func (f *eventFollowups) releasedPush(ctx context.Context, eventID string) (*replicationPushFollowupPayload, error) {
	var raw string
	err := f.store.DB().QueryRowContext(ctx, `SELECT payload FROM work_intents WHERE id=?`, storage.IntentID(eventFollowupKind, eventID, "stored")).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var payload eventFollowupPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, err
	}
	return payload.Push, nil
}

// Discovery is retried only when the original local lookup failed. The
// acceptance cutoff prevents a new registration from receiving old events.
func (f *eventFollowups) callbackIntents(ctx context.Context, payload eventFollowupPayload) ([]storage.Intent, error) {
	if payload.DiscoverCallbacks {
		ids, err := f.callbacks.CandidateIDs(ctx, payload.Event)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			candidate, err := f.callbacks.callback(ctx, id)
			if err != nil {
				return nil, err
			}
			if candidate.CreatedAt < payload.AcceptedAt {
				payload.Callbacks = append(payload.Callbacks, id)
			}
		}
	}
	return f.callbacks.PrepareFor(ctx, payload.Event, payload.Callbacks)
}

func (f *eventFollowups) viewIntents(ctx context.Context, payload eventFollowupPayload) ([]storage.Intent, error) {
	if payload.DiscoverViews {
		targets, err := f.views.CandidateTargets(ctx, payload.Event)
		if err != nil {
			return nil, err
		}
		for _, target := range targets {
			candidate, err := f.views.customViewByName(ctx, target.Name)
			if err != nil {
				return nil, err
			}
			if candidate.CreatedAt < payload.AcceptedAt {
				payload.Views = append(payload.Views, target)
			}
		}
	}
	return f.views.PrepareTargets(ctx, payload.Event, payload.Views, payload.Released)
}

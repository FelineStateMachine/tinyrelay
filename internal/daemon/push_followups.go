package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type replicationPushFollowupPayload struct {
	Event              event.Event `json:"event"`
	AcceptedAt         int64       `json:"accepted_at"`
	RegistrationIDs    []string    `json:"registration_ids,omitempty"`
	RegistrationCutoff int64       `json:"registration_cutoff,omitempty"`
	Discover           bool        `json:"discover,omitempty"`
}

// prepareReplicationPushFollowup captures registrations that existed when the
// source event was accepted. RegistrationCutoff is the SQLite insertion
// sequence, rather than the registration event's authored timestamp; clients
// can author addressable registrations with arbitrary timestamps. A discovery
// error is represented in the payload so the shared event-followup worker can
// retry it after the source event commits.
func (t *Tenant) prepareReplicationPushFollowup(ctx context.Context, e event.Event, now int64) (replicationPushFollowupPayload, error) {
	payload := replicationPushFollowupPayload{Event: e, AcceptedAt: now}
	ids, cutoff, err := t.captureReplicationPushRegistrations(ctx, e)
	payload.RegistrationIDs, payload.RegistrationCutoff, payload.Discover = ids, cutoff, err != nil
	return payload, nil
}

// recoverReplicationPushFollowup discovers only registrations inserted no
// later than the acceptance cutoff. This prevents a registration created while
// the worker was retrying from receiving an older event.
func (t *Tenant) recoverReplicationPushFollowup(ctx context.Context, payload *replicationPushFollowupPayload) error {
	if !payload.Discover {
		return nil
	}
	ids, err := t.discoverReplicationPushRegistrations(ctx, payload.Event, payload.RegistrationCutoff)
	if err != nil {
		return err
	}
	payload.RegistrationIDs = appendUniqueStrings(payload.RegistrationIDs, ids...)
	payload.Discover = false
	return nil
}

func (t *Tenant) prepareReplicationPushIntents(ctx context.Context, payload replicationPushFollowupPayload) ([]storage.Intent, error) {
	registrations, err := t.loadReplicationPushRegistrations(ctx, payload.RegistrationIDs, payload.RegistrationCutoff)
	if err != nil {
		return nil, err
	}
	return replication.PrepareCallbackIntents(ctx, payload.Event, registrations), nil
}

func (t *Tenant) planReplicationPushFollowup(ctx context.Context, payload replicationPushFollowupPayload) ([]storage.Intent, error) {
	if !t.Policy().Features.Push {
		return nil, nil
	}
	if err := t.recoverReplicationPushFollowup(ctx, &payload); err != nil {
		return nil, err
	}
	return t.prepareReplicationPushIntents(ctx, payload)
}

func (t *Tenant) captureReplicationPushRegistrations(ctx context.Context, e event.Event) ([]string, int64, error) {
	var cutoff int64
	if err := t.store.DB().QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0) FROM events").Scan(&cutoff); err != nil {
		return nil, 0, err
	}
	return t.discoverReplicationPushRegistrationsAt(ctx, e, cutoff)
}

func (t *Tenant) discoverReplicationPushRegistrations(ctx context.Context, e event.Event, cutoff int64) ([]string, error) {
	if cutoff <= 0 {
		return nil, nil
	}
	ids, _, err := t.discoverReplicationPushRegistrationsAt(ctx, e, cutoff)
	return ids, err
}

func (t *Tenant) discoverReplicationPushRegistrationsAt(ctx context.Context, e event.Event, cutoff int64) ([]string, int64, error) {
	rows, err := t.store.DB().QueryContext(ctx, `SELECT seq,id,raw FROM events WHERE kind=? AND seq<=? ORDER BY seq`, event.KIND_PUSH_REGISTRATION, cutoff)
	if err != nil {
		return nil, cutoff, err
	}
	defer rows.Close()
	approved := t.callbackPolicy()
	ids := []string{}
	for rows.Next() {
		var seq int64
		var id, raw string
		if err := rows.Scan(&seq, &id, &raw); err != nil {
			return ids, cutoff, err
		}
		var registration event.Event
		if json.Unmarshal([]byte(raw), &registration) != nil {
			continue
		}
		parsed, parseErr := replication.ParsePushRegistration(registration, approved, t.RelayURL())
		if parseErr == nil && replication.PushMatches(parsed, e) {
			ids = append(ids, id)
		}
	}
	return ids, cutoff, rows.Err()
}

func (t *Tenant) loadReplicationPushRegistrations(ctx context.Context, ids []string, cutoff int64) ([]replication.PushRegistration, error) {
	approved := t.callbackPolicy()
	registrations := make([]replication.PushRegistration, 0, len(ids))
	for _, id := range ids {
		var seq int64
		var raw string
		err := t.store.DB().QueryRowContext(ctx, `SELECT seq,raw FROM events WHERE id=? AND kind=?`, id, event.KIND_PUSH_REGISTRATION).Scan(&seq, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if cutoff > 0 && seq > cutoff {
			continue
		}
		var registration event.Event
		if json.Unmarshal([]byte(raw), &registration) != nil {
			continue
		}
		parsed, parseErr := replication.ParsePushRegistration(registration, approved, t.RelayURL())
		if parseErr == nil {
			registrations = append(registrations, parsed)
		}
	}
	return registrations, nil
}

func appendUniqueStrings(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	for _, value := range additions {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values
}

package records

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

func (s *Service) pins(ctx context.Context, now int64) (event.Event, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT ref FROM records_pins ORDER BY position`)
	if err != nil {
		return event.Event{}, err
	}
	defer rows.Close()
	tags := [][]string{{"-"}, {"d", s.group()}}
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return event.Event{}, err
		}
		tags = append(tags, []string{"e", ref})
	}
	if err := rows.Err(); err != nil {
		return event.Event{}, err
	}
	return s.signed(ctx, event.KIND_GROUP_PINS, tags, "", now)
}

// PublishMembership emits relay-owned identity and group records deterministically.
func (s *Service) PublishMembership(ctx context.Context, now int64) ([]event.Event, error) {
	var out []event.Event
	for _, fn := range []func(context.Context, int64) (event.Event, error){s.profile, s.discovery, s.roster, s.groupState} {
		e, err := fn(ctx, now)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	group, err := s.groupRecords(ctx, now)
	if err != nil {
		return nil, err
	}
	out = append(out, group...)
	return out, nil
}

// PublishMembershipChanges extends the projection with membership changes.
func (s *Service) PublishMembershipChanges(ctx context.Context, changes []MembershipChange, now int64) ([]event.Event, error) {
	out, err := s.PublishMembership(ctx, now)
	if err != nil {
		return nil, err
	}
	for _, c := range changes {
		if c.Added != nil {
			d, err := s.MembershipDelta(ctx, c.PubKey, *c.Added, now)
			if err != nil {
				return nil, err
			}
			out = append(out, d)
		}
		kind := event.KIND_PUT_USER
		if c.Added != nil && !*c.Added {
			kind = event.KIND_REMOVE_USER
		}
		tags := [][]string{{"-"}, {"h", s.group()}, {"p", c.PubKey}}
		for _, role := range c.Roles {
			tags[1] = append(tags[1], role)
		}
		e, err := s.signed(ctx, kind, tags, "", now)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *Service) RecordPresence(ctx context.Context, pubkey string, now int64) error {
	if pubkey == "" {
		return errors.New("presence: empty pubkey")
	}
	_, err := s.store.DB().ExecContext(ctx, `INSERT INTO records_presence(pubkey,seen_at) VALUES(?,?) ON CONFLICT(pubkey) DO UPDATE SET seen_at=excluded.seen_at`, pubkey, now)
	return err
}

// NotePresence records presence from authenticated host paths.
func (s *Service) NotePresence(ctx context.Context, pubkey string, now int64) error {
	return s.RecordPresence(ctx, pubkey, now)
}

func (s *Service) Presence(ctx context.Context, caller policy.Access, now int64) (event.Event, error) {
	if !policy.CanRead(s.policy(), event.Event{Kind: event.KIND_PRESENCE}, caller) {
		return event.Event{}, errors.New("restricted: presence")
	}
	rows, err := s.store.DB().QueryContext(ctx, `SELECT pubkey,seen_at FROM records_presence WHERE seen_at>=? ORDER BY pubkey`, now-15*60)
	if err != nil {
		return event.Event{}, err
	}
	defer rows.Close()
	var values []Presence
	for rows.Next() {
		var p Presence
		if err := rows.Scan(&p.PubKey, &p.SeenAt); err != nil {
			return event.Event{}, err
		}
		values = append(values, p)
	}
	b, _ := json.Marshal(values)
	return s.signedOnly(ctx, event.KIND_PRESENCE, nil, string(b), now)
}

// SetPins persists ordered references and emits the signed pin record.
func (s *Service) SetPins(ctx context.Context, refs []string, now int64) (event.Event, error) {
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		return s.ReplacePinsTx(ctx, tx, refs)
	})
	if err != nil {
		return event.Event{}, err
	}
	return s.pins(ctx, now)
}

// ReplacePinsTx replaces the records-owned pin list in a caller-owned
// transaction. Callers use this when pin metadata and its projection must
// commit together.
func (s *Service) ReplacePinsTx(ctx context.Context, tx *sql.Tx, refs []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM records_pins`); err != nil {
		return err
	}
	for position, ref := range refs {
		if strings.TrimSpace(ref) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO records_pins(position,ref) VALUES(?,?)`, position, ref); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) audit(ctx context.Context, actor, action, target, detail string, now int64) error {
	_, err := s.store.DB().ExecContext(ctx, `INSERT INTO records_audit(at,actor,action,target,detail) VALUES(?,?,?,?,?)`, now, actor, action, target, detail)
	return err
}

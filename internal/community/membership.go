package community

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// EnqueueProjectionTx records a durable records-projection intent in a caller
// owned transaction. Root event ingestion should call this before storing the
// accepted event, so a failed event write rolls back both the projection work
// and membership side effects. The intent is idempotent by kind/event/target.
func (s *Service) EnqueueProjectionTx(ctx context.Context, tx *sql.Tx, ev event.Event) error {
	id := ev.ID
	if id == "" {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", ev.PubKey, ev.Kind, ev.Content)))
		id = fmt.Sprintf("community-event-%x", sum[:])
	}
	payload, err := json.Marshal(map[string]any{"kind": ev.Kind, "pubkey": ev.PubKey, "id": ev.ID})
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO work_intents(id,kind,event_id,target,payload,next_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, id, "records-projection", id, "membership", string(payload), now, now, now)
	return err
}

// EnqueueProjection is the non-composable convenience form. Use
// EnqueueProjectionTx when event persistence must share the transaction.
func (s *Service) EnqueueProjection(ctx context.Context, ev event.Event) error {
	return s.store.WithTx(ctx, func(tx *sql.Tx) error { return s.EnqueueProjectionTx(ctx, tx, ev) })
}

// HandleProjectionEventTx is the transaction hook for records-owned group
// metadata and pins (9002 and 9010). The caller applies its records state and
// stores the accepted event from the same transaction; this package adds the
// durable projection intent after the side effect succeeds.
func (s *Service) HandleProjectionEventTx(ctx context.Context, tx *sql.Tx, ev event.Event, sideEffect func(*sql.Tx) error) error {
	if ev.Kind != event.KIND_EDIT_METADATA && ev.Kind != event.KIND_PINS {
		return fmt.Errorf("unsupported: projection event kind %d", ev.Kind)
	}
	if sideEffect == nil {
		return errors.New("community: nil projection side effect")
	}
	if err := sideEffect(tx); err != nil {
		return err
	}
	return s.EnqueueProjectionTx(ctx, tx, ev)
}

// MembershipResult describes a NIP-29/NIP-43 management event. Membership
// events are projected into community tables and are not themselves stored by
// this package; the relay decides whether to retain or fan them out.
type MembershipResult struct {
	OK      bool
	Message string
	Stored  bool
}

// HandleMembershipEventTx applies join/leave admission and invokes persist in
// the same SQLite transaction. The callback must store the accepted event (or
// return an error); membership and projection work then roll back together.
// Management events that need records-owned state should use the root's
// transaction hooks and EnqueueProjectionTx directly.
func (s *Service) HandleMembershipEventTx(ctx context.Context, ev event.Event, persist func(*sql.Tx) error) (MembershipResult, error) {
	if persist == nil {
		return MembershipResult{}, errors.New("community: nil event persistence callback")
	}
	if ev.Kind != event.KIND_JOIN && ev.Kind != event.KIND_NIP43_JOIN && ev.Kind != event.KIND_LEAVE && ev.Kind != event.KIND_NIP43_LEAVE {
		return MembershipResult{}, fmt.Errorf("unsupported: transactional membership event kind %d", ev.Kind)
	}
	var out MembershipResult
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM community_members WHERE pubkey=?)`, ev.PubKey).Scan(&exists); err != nil {
			return err
		}
		if ev.Kind == event.KIND_LEAVE || ev.Kind == event.KIND_NIP43_LEAVE {
			if ev.PubKey == s.owner {
				return errors.New("restricted: the owner cannot leave")
			}
			if !exists {
				if ev.Kind == event.KIND_NIP43_LEAVE {
					out = MembershipResult{OK: true, Message: "duplicate: not a member", Stored: false}
					return nil
				}
				return errors.New("invalid: not a member")
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM community_members WHERE pubkey=?`, ev.PubKey); err != nil {
				return err
			}
			out = MembershipResult{OK: true, Message: "info: access revoked", Stored: ev.Kind == event.KIND_LEAVE}
		} else {
			if exists {
				out = MembershipResult{OK: true, Message: "duplicate: already a member", Stored: false}
				return nil
			}
			claim := tagValue(ev, "code")
			if ev.Kind == event.KIND_NIP43_JOIN {
				claim = tagValue(ev, "claim")
			}
			if claim == "" {
				if ev.Kind == event.KIND_NIP43_JOIN || s.currentPolicy().Writes != "open" {
					return errors.New("restricted: a join request needs an invite claim")
				}
				if err := checkTerms(s.currentPolicy().JoinTerms, tagValue(ev, "terms")); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO community_members(pubkey,role,via,created_at) VALUES(?,?,?,?)`, ev.PubKey, "member", "join", now()); err != nil {
					return err
				}
			} else {
				var creator string
				var exp int64
				var max, uses int
				if err := tx.QueryRowContext(ctx, `SELECT created_by,expires_at,max_uses,uses FROM community_invites WHERE code=?`, claim).Scan(&creator, &exp, &max, &uses); errors.Is(err, sql.ErrNoRows) {
					return errors.New("invite_invalid")
				} else if err != nil {
					return err
				}
				if exp < now() {
					return errors.New("invite_expired")
				}
				if max > 0 && uses >= max {
					return errors.New("invite_exhausted")
				}
				var creatorExists bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM community_members WHERE pubkey=?)`, creator).Scan(&creatorExists); err != nil {
					return err
				}
				if !creatorExists {
					return errors.New("invite_revoked")
				}
				if err := checkTerms(s.currentPolicy().JoinTerms, tagValue(ev, "terms")); err != nil {
					return err
				}
				res, err := tx.ExecContext(ctx, `UPDATE community_invites SET uses=uses+1 WHERE code=? AND (max_uses=0 OR uses<max_uses)`, claim)
				if err != nil {
					return err
				}
				n, _ := res.RowsAffected()
				if n != 1 {
					return errors.New("invite_exhausted")
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO community_members(pubkey,role,invited_by,via,created_at) VALUES(?,?,?,?,?)`, ev.PubKey, "member", creator, "invite "+claim, now()); err != nil {
					return err
				}
			}
			out = MembershipResult{OK: true, Message: "info: welcome", Stored: ev.Kind == event.KIND_JOIN}
		}
		if err := s.EnqueueProjectionTx(ctx, tx, ev); err != nil {
			return err
		}
		if out.Stored {
			return persist(tx)
		}
		return nil
	})
	return out, err
}

// HandleModerationEventTx applies NIP-29 moderation side effects and lets the
// root store the accepted event in the same transaction. It covers put-user,
// remove-user, delete-event and create-invite. Metadata and pins are owned by
// records; roots should call EnqueueProjectionTx from their transaction.
func (s *Service) HandleModerationEventTx(ctx context.Context, ev event.Event, persist func(*sql.Tx) error) (MembershipResult, error) {
	if persist == nil {
		return MembershipResult{}, errors.New("community: nil event persistence callback")
	}
	if ev.Kind != event.KIND_PUT_USER && ev.Kind != event.KIND_REMOVE_USER && ev.Kind != event.KIND_DELETE_EVENT && ev.Kind != event.KIND_CREATE_INVITE {
		return MembershipResult{}, fmt.Errorf("unsupported: transactional moderation event kind %d", ev.Kind)
	}
	role, err := s.Role(ctx, ev.PubKey)
	if err != nil {
		return MembershipResult{}, err
	}
	if role != "owner" && role != "moderator" && !(ev.Kind == event.KIND_CREATE_INVITE && role == "member") {
		return MembershipResult{}, errors.New("restricted: not a group admin")
	}
	var out MembershipResult
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		target := tagValue(ev, "p")
		switch ev.Kind {
		case event.KIND_PUT_USER, event.KIND_REMOVE_USER:
			if !validPubKey(target) {
				return errors.New("invalid: membership event needs a p tag")
			}
			if target == s.owner {
				return errors.New("invalid: the owner role cannot change")
			}
			var old string
			_ = tx.QueryRowContext(ctx, `SELECT role FROM community_members WHERE pubkey=?`, target).Scan(&old)
			if old == "moderator" && role != "owner" {
				return errors.New("restricted: only the owner changes moderators")
			}
			if ev.Kind == event.KIND_REMOVE_USER {
				if _, err := tx.ExecContext(ctx, `DELETE FROM community_members WHERE pubkey=?`, target); err != nil {
					return err
				}
			} else {
				wantsMod := false
				for _, tag := range ev.Tags {
					if len(tag) > 2 && tag[0] == "p" && tag[1] == target && tag[2] == "moderator" {
						wantsMod = true
					}
				}
				if wantsMod && role != "owner" {
					return errors.New("restricted: only the owner appoints moderators")
				}
				next := "member"
				if wantsMod {
					next = "moderator"
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO community_members(pubkey,role,via,created_at) VALUES(?,?,?,?) ON CONFLICT(pubkey) DO UPDATE SET role=excluded.role`, target, next, "put-user", now()); err != nil {
					return err
				}
			}
		case event.KIND_DELETE_EVENT:
			ids := []string{}
			for _, tag := range ev.Tags {
				if len(tag) > 1 && tag[0] == "e" && validPubKey(tag[1]) {
					ids = append(ids, tag[1])
				}
			}
			if len(ids) == 0 {
				return errors.New("invalid: delete-event needs an e tag")
			}
			for _, id := range ids {
				if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE id=?`, id); err != nil {
					return err
				}
			}
		case event.KIND_CREATE_INVITE:
			if role == "member" {
				if reason := s.memberInviteGateTx(ctx, tx, ev.PubKey); reason != "" {
					return errors.New(reason)
				}
			}
			code := tagValue(ev, "code")
			if code == "" {
				var e error
				code, e = token()
				if e != nil {
					return e
				}
			} else if !codeRE.MatchString(code) {
				return errors.New("invalid: invite code")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO community_invites(code,created_by,created_at,expires_at,max_uses,note) VALUES(?,?,?,?,?,?)`, code, ev.PubKey, now(), now()+3*86400, 0, ev.Content[:min(200, len(ev.Content))]); err != nil {
				return err
			}
		}
		if err := s.EnqueueProjectionTx(ctx, tx, ev); err != nil {
			return err
		}
		out = MembershipResult{OK: true, Stored: true}
		return persist(tx)
	})
	return out, err
}

// OnMembershipApplied is retained as a source compatibility shim. Projection
// refresh is now driven by durable records-projection work intents; invoking a
// relay callback here can deadlock a publish fence.
func (s *Service) OnMembershipApplied(fn func(event.Event)) {
	_ = fn
}

// HandleMembershipEvent applies the NIP-29 group management subset and the
// NIP-43 join/leave requests. The relay's group id is checked by the caller;
// this service only owns membership and moderation state.
func (s *Service) HandleMembershipEvent(ctx context.Context, ev event.Event) (MembershipResult, error) {
	if ev.PubKey == "" || !validPubKey(ev.PubKey) {
		return MembershipResult{}, errors.New("invalid: event pubkey")
	}
	var result MembershipResult
	switch ev.Kind {
	case event.KIND_JOIN, event.KIND_NIP43_JOIN:
		claim := tagValue(ev, "code")
		if ev.Kind == event.KIND_NIP43_JOIN {
			claim = tagValue(ev, "claim")
		}
		terms := tagValue(ev, "terms")
		if claim == "" {
			if ev.Kind == event.KIND_NIP43_JOIN || s.currentPolicy().Writes != "open" {
				return MembershipResult{}, errors.New("restricted: a join request needs an invite claim")
			}
			if s.currentPolicy().JoinTerms != "" {
				sum := sha256.Sum256([]byte(s.currentPolicy().JoinTerms))
				if !strings.EqualFold(hex.EncodeToString(sum[:]), terms) {
					return MembershipResult{}, errors.New("terms_mismatch")
				}
			}
			if _, err := s.setMember(ctx, ev.PubKey, []json.RawMessage{mustJSON(ev.PubKey), mustJSON(map[string]any{"role": "member", "via": "join"})}); err != nil {
				return MembershipResult{}, err
			}
			result = MembershipResult{OK: true, Stored: ev.Kind == event.KIND_JOIN}
			break
		}
		if _, err := s.ClaimWithTerms(ctx, claim, ev.PubKey, terms); err != nil {
			return MembershipResult{}, err
		}
		result = MembershipResult{OK: true, Message: "info: welcome", Stored: ev.Kind == event.KIND_JOIN}
	case event.KIND_LEAVE, event.KIND_NIP43_LEAVE:
		if ev.PubKey == s.owner {
			return MembershipResult{}, errors.New("restricted: the owner cannot leave")
		}
		if role, _ := s.Role(ctx, ev.PubKey); role == "" {
			if ev.Kind == event.KIND_NIP43_LEAVE {
				return MembershipResult{OK: true, Message: "duplicate: not a member", Stored: false}, nil
			}
			return MembershipResult{}, errors.New("invalid: not a member")
		}
		res, err := s.removeMember(ctx, ev.PubKey, []json.RawMessage{mustJSON(ev.PubKey)})
		if err != nil {
			return MembershipResult{}, err
		}
		_ = res
		result = MembershipResult{OK: true, Message: "info: access revoked", Stored: ev.Kind == event.KIND_LEAVE}
	case event.KIND_PUT_USER, event.KIND_REMOVE_USER, event.KIND_EDIT_METADATA, event.KIND_DELETE_EVENT, event.KIND_CREATE_INVITE, event.KIND_PINS:
		result, _ = s.handleModerationEvent(ctx, ev)
	default:
		return MembershipResult{}, fmt.Errorf("unsupported: membership event kind %d", ev.Kind)
	}
	return result, nil
}

func tagValue(ev event.Event, name string) string {
	for _, tag := range ev.Tags {
		if len(tag) > 1 && tag[0] == name {
			return tag[1]
		}
	}
	return ""
}

func checkTerms(terms, supplied string) error {
	if terms == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(terms))
	if !strings.EqualFold(hex.EncodeToString(sum[:]), supplied) {
		return errors.New("terms_mismatch")
	}
	return nil
}

func (s *Service) handleModerationEvent(ctx context.Context, ev event.Event) (MembershipResult, error) {
	role, err := s.Role(ctx, ev.PubKey)
	if err != nil {
		return MembershipResult{}, err
	}
	if role != "owner" && role != "moderator" {
		return MembershipResult{}, errors.New("restricted: not a group admin")
	}
	target := tagValue(ev, "p")
	if ev.Kind == event.KIND_PUT_USER || ev.Kind == event.KIND_REMOVE_USER {
		if !validPubKey(target) {
			return MembershipResult{}, errors.New("invalid: membership event needs a p tag")
		}
		if target == s.owner {
			return MembershipResult{}, errors.New("invalid: the owner role cannot change")
		}
		old, _ := s.Role(ctx, target)
		if old == "moderator" && role != "owner" {
			return MembershipResult{}, errors.New("restricted: only the owner changes moderators")
		}
		if ev.Kind == event.KIND_REMOVE_USER {
			if _, err := s.removeMember(ctx, target, []json.RawMessage{mustJSON(target)}); err != nil {
				return MembershipResult{}, err
			}
		} else {
			wantsMod := false
			for _, tag := range ev.Tags {
				if len(tag) > 2 && tag[0] == "p" && tag[1] == target && tag[2] == "moderator" {
					wantsMod = true
				}
			}
			if wantsMod && role != "owner" {
				return MembershipResult{}, errors.New("restricted: only the owner appoints moderators")
			}
			memberRole := "member"
			if wantsMod {
				memberRole = "moderator"
			}
			if _, err := s.setMember(ctx, ev.PubKey, []json.RawMessage{mustJSON(target), mustJSON(map[string]any{"role": memberRole, "via": "put-user"})}); err != nil {
				return MembershipResult{}, err
			}
		}
		return MembershipResult{OK: true, Stored: true}, nil
	}
	if ev.Kind == event.KIND_CREATE_INVITE {
		_, err := s.createInvite(ctx, ev.PubKey, []json.RawMessage{mustJSON(int64(0)), mustJSON(0), mustJSON(ev.Content)}, false)
		if err != nil {
			return MembershipResult{}, err
		}
		return MembershipResult{OK: true, Stored: true}, nil
	}
	if ev.Kind == event.KIND_DELETE_EVENT {
		ids := []string{}
		for _, tag := range ev.Tags {
			if len(tag) > 1 && tag[0] == "e" && validPubKey(tag[1]) {
				ids = append(ids, tag[1])
			}
		}
		if len(ids) == 0 {
			return MembershipResult{}, errors.New("invalid: delete-event needs an e tag")
		}
		if err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
			for _, id := range ids {
				if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE id=?`, id); err != nil {
					return err
				}
			}
			return s.recordTx(ctx, tx, ev.PubKey, "delete-event", strings.Join(ids, ","), "")
		}); err != nil {
			return MembershipResult{}, err
		}
		return MembershipResult{OK: true, Stored: true}, nil
	}
	if ev.Kind == event.KIND_EDIT_METADATA || ev.Kind == event.KIND_PINS {
		return MembershipResult{}, errors.New("unsupported: metadata projection is owned by relay root")
	}
	return MembershipResult{}, fmt.Errorf("unsupported: membership event kind %d", ev.Kind)
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

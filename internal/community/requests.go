package community

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// A NIP-43 join (kind 28934) without a claim tag is an access request: the
// relay holds it for the owner and moderators instead of refusing it. The
// request is a row, never a stored event, and one key holds one row that a
// repeat refreshes.

const (
	// JoinReasonMax bounds the reason kept from the request's content.
	JoinReasonMax = 500
	// JoinRequestReceived is the OK reason answered to an access request.
	JoinRequestReceived = "info: access request received, the relay owner will review it"
)

// IsAccessRequest reports whether a membership event asks for access rather
// than claiming an invite.
func IsAccessRequest(ev event.Event) bool {
	return ev.Kind == event.KIND_NIP43_JOIN && tagValue(ev, "claim") == ""
}

// JoinReason is the request content trimmed of surrounding space and cut to
// JoinReasonMax characters on a character boundary.
func JoinReason(content string) string {
	reason := strings.TrimSpace(content)
	if utf8.RuneCountInString(reason) <= JoinReasonMax {
		return reason
	}
	count := 0
	for i := range reason {
		if count == JoinReasonMax {
			return strings.TrimSpace(reason[:i])
		}
		count++
	}
	return reason
}

// recordJoinRequestTx stores or refreshes the key's access request and
// reports whether a new pending request was created. A pending request from
// the same key takes the new reason and time; a decided one becomes pending
// again. A banned key is refused.
func (s *Service) recordJoinRequestTx(ctx context.Context, tx *sql.Tx, pubkey, reason string) (bool, error) {
	var banned bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM community_bans WHERE pubkey=?)`, pubkey).Scan(&banned); err != nil {
		return false, err
	}
	if banned {
		return false, errors.New("restricted: this key is banned from the relay")
	}
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM join_requests WHERE pubkey=?`, pubkey).Scan(&status)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO join_requests(pubkey,reason,requested_at,status,decided_by,decided_at) VALUES(?,?,?,'pending','',0) ON CONFLICT(pubkey) DO UPDATE SET reason=excluded.reason,requested_at=excluded.requested_at,status='pending',decided_by='',decided_at=0`, pubkey, reason, now()); err != nil {
		return false, err
	}
	return status != "pending", nil
}

// RecordJoinRequest is the non-transactional form of recordJoinRequestTx.
func (s *Service) RecordJoinRequest(ctx context.Context, pubkey, reason string) (bool, error) {
	created := false
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		created, err = s.recordJoinRequestTx(ctx, tx, pubkey, reason)
		return err
	})
	return created, err
}

// listJoinRequests lists pending requests newest first, then the decided
// ones newest decision first.
func (s *Service) listJoinRequests(ctx context.Context) (any, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT pubkey,reason,requested_at,status,decided_by,decided_at FROM join_requests ORDER BY status<>'pending', CASE WHEN status='pending' THEN requested_at ELSE decided_at END DESC, pubkey LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JoinRequest{}
	for rows.Next() {
		var r JoinRequest
		if err := rows.Scan(&r.PubKey, &r.Reason, &r.RequestedAt, &r.Status, &r.DecidedBy, &r.DecidedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PendingJoinRequests counts the requests that wait for a decision.
func (s *Service) PendingJoinRequests(ctx context.Context) (int, error) {
	var count int
	err := s.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM join_requests WHERE status='pending'`).Scan(&count)
	return count, err
}

func (s *Service) joinRequestTarget(ctx context.Context, p []json.RawMessage) (string, string, error) {
	var pk string
	if err := decode(p, 0, &pk); err != nil {
		return "", "", err
	}
	if !validPubKey(pk) {
		return "", "", errors.New("invalid: bad pubkey")
	}
	var status string
	err := s.store.DB().QueryRowContext(ctx, `SELECT status FROM join_requests WHERE pubkey=?`, pk).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errors.New("not found: no access request from that key")
	}
	return pk, status, err
}

// approveJoin admits the key through the member path setmember uses, so the
// signed member list and add-user record follow, and marks the request
// approved. A key that already became a member only has its request marked.
func (s *Service) approveJoin(ctx context.Context, actor string, p []json.RawMessage) (any, error) {
	pk, _, err := s.joinRequestTarget(ctx, p)
	if err != nil {
		return nil, err
	}
	role, err := s.Role(ctx, pk)
	if err != nil {
		return nil, err
	}
	if role == "" {
		if _, err := s.setMemberVia(ctx, actor, []json.RawMessage{mustJSON(pk), mustJSON(map[string]any{"role": "member"})}, "request"); err != nil {
			return nil, err
		}
	}
	return s.decideJoin(ctx, actor, pk, "approved")
}

// denyJoin marks the request denied. The key stays outside the relay and
// may ask again.
func (s *Service) denyJoin(ctx context.Context, actor string, p []json.RawMessage) (any, error) {
	pk, status, err := s.joinRequestTarget(ctx, p)
	if err != nil {
		return nil, err
	}
	if status == "approved" {
		return nil, errors.New("invalid: the request was approved; remove the member instead")
	}
	return s.decideJoin(ctx, actor, pk, "denied")
}

func (s *Service) decideJoin(ctx context.Context, actor, pk, status string) (any, error) {
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE join_requests SET status=?,decided_by=?,decided_at=? WHERE pubkey=?`, status, actor, now(), pk); err != nil {
			return err
		}
		action := "denyjoin"
		if status == "approved" {
			action = "approvejoin"
		}
		return s.recordTx(ctx, tx, actor, action, pk, "")
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"pubkey": pk, "status": status}, nil
}

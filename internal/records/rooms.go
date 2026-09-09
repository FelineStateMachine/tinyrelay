package records

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// PublishRoom emits the relay-signed metadata (39000), admins (39001) and
// members (39002) records for one room. A room that no longer exists has its
// records retired instead. The slug room is covered by PublishMembership.
func (s *Service) PublishRoom(ctx context.Context, roomID string, now int64) ([]event.Event, error) {
	var name, about, picture, access string
	var deleted int64
	err := s.store.DB().QueryRowContext(ctx, `SELECT name,about,picture,access,deleted_at FROM rooms WHERE id=?`, roomID).Scan(&name, &about, &picture, &access, &deleted)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && deleted > 0) {
		return nil, s.RetireRoom(ctx, roomID)
	}
	if err != nil {
		return nil, fmt.Errorf("room record: %w", err)
	}
	rows, err := s.store.DB().QueryContext(ctx, `SELECT pubkey,role FROM room_members WHERE room_id=? ORDER BY CASE role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END,pubkey`, roomID)
	if err != nil {
		return nil, fmt.Errorf("room members: %w", err)
	}
	defer rows.Close()
	var admins, members [][]string
	for rows.Next() {
		var pk, role string
		if err := rows.Scan(&pk, &role); err != nil {
			return nil, err
		}
		members = append(members, []string{"p", pk})
		if role == "owner" || role == "admin" {
			admins = append(admins, []string{"p", pk, role})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	p := s.policy()
	if access == "open" && !p.DirectoryPublic {
		members = nil
	}
	meta := [][]string{{"-"}, {"d", roomID}, {"name", name}, {"about", about}}
	if picture != "" {
		meta = append(meta, []string{"picture", picture})
	}
	if access == "members" {
		meta = append(meta, []string{"closed"})
	} else {
		meta = append(meta, []string{"open"})
	}
	if p.Reads == "members" {
		meta = append(meta, []string{"private"})
	} else {
		meta = append(meta, []string{"public"})
	}
	vals := []struct {
		kind int
		tags [][]string
	}{
		{event.KIND_GROUP_METADATA, meta},
		{event.KIND_GROUP_ADMINS, append([][]string{{"-"}, {"d", roomID}}, admins...)},
		{event.KIND_GROUP_MEMBERS, append([][]string{{"-"}, {"d", roomID}}, members...)},
	}
	out := make([]event.Event, 0, len(vals))
	for _, v := range vals {
		e, err := s.signed(ctx, v.kind, v.tags, "", now)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// RoomNotice emits the relay-signed member notice a Buzz-compatible client
// shows when someone is added to (44100) or removed from (44101) a room.
func (s *Service) RoomNotice(ctx context.Context, roomID, pubkey string, added bool, now int64) (event.Event, error) {
	kind := event.KIND_ROOM_MEMBER_REMOVED
	if added {
		kind = event.KIND_ROOM_MEMBER_ADDED
	}
	return s.signed(ctx, kind, [][]string{{"h", roomID}, {"p", pubkey}}, "", now)
}

// RetireRoom removes the relay's own records for a room that was deleted.
func (s *Service) RetireRoom(ctx context.Context, roomID string) error {
	_, err := s.store.DB().ExecContext(ctx, `DELETE FROM events WHERE pubkey=? AND kind IN (?,?,?) AND d=?`, s.PublicKey(), event.KIND_GROUP_METADATA, event.KIND_GROUP_ADMINS, event.KIND_GROUP_MEMBERS, roomID)
	return err
}

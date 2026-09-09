package community

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// Rooms are NIP-29 groups that live inside one tenant. The room whose id
// equals the tenant slug mirrors the tenant roster; every other room keeps
// its own member list with room-level roles. Membership rows are kept in
// step with the tenant roster by triggers so every roster path (management
// calls, invites, NIP-29 and NIP-43 events, owner transfer) is covered.
const roomSchema = `
CREATE TABLE IF NOT EXISTS rooms(id TEXT PRIMARY KEY,name TEXT NOT NULL DEFAULT '',about TEXT NOT NULL DEFAULT '',picture TEXT NOT NULL DEFAULT '',access TEXT NOT NULL DEFAULT 'open',created_by TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,event_id TEXT NOT NULL DEFAULT '',deleted_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS room_members(room_id TEXT NOT NULL,pubkey TEXT NOT NULL,role TEXT NOT NULL DEFAULT 'member',added_at INTEGER NOT NULL,PRIMARY KEY(room_id,pubkey));
CREATE INDEX IF NOT EXISTS room_members_pubkey ON room_members(pubkey);
CREATE TRIGGER IF NOT EXISTS room_mirror_insert AFTER INSERT ON community_members
BEGIN
 INSERT INTO room_members(room_id,pubkey,role,added_at)
 SELECT value,NEW.pubkey,CASE NEW.role WHEN 'owner' THEN 'owner' WHEN 'moderator' THEN 'admin' ELSE 'member' END,NEW.created_at FROM community_meta WHERE key='rooms.slug'
 ON CONFLICT(room_id,pubkey) DO UPDATE SET role=excluded.role WHERE role<>excluded.role;
END;
CREATE TRIGGER IF NOT EXISTS room_mirror_update AFTER UPDATE OF role ON community_members
BEGIN
 UPDATE room_members SET role=CASE NEW.role WHEN 'owner' THEN 'owner' WHEN 'moderator' THEN 'admin' ELSE 'member' END
 WHERE pubkey=NEW.pubkey AND room_id IN (SELECT value FROM community_meta WHERE key='rooms.slug') AND role<>CASE NEW.role WHEN 'owner' THEN 'owner' WHEN 'moderator' THEN 'admin' ELSE 'member' END;
END;
CREATE TRIGGER IF NOT EXISTS room_mirror_delete AFTER DELETE ON community_members
BEGIN
 DELETE FROM room_members WHERE pubkey=OLD.pubkey;
END;
CREATE TRIGGER IF NOT EXISTS room_member_projection_insert AFTER INSERT ON room_members
BEGIN
 INSERT OR IGNORE INTO work_intents(id,kind,event_id,target,payload,next_at,created_at,updated_at)
 VALUES('room-member-'||lower(hex(randomblob(16))),'records-projection','room-member-'||lower(hex(randomblob(16))),'room',json_object('action','room','room',NEW.room_id,'pubkey',NEW.pubkey,'role',NEW.role),strftime('%s','now'),strftime('%s','now'),strftime('%s','now'));
END;
CREATE TRIGGER IF NOT EXISTS room_member_projection_update AFTER UPDATE OF role ON room_members
BEGIN
 INSERT OR IGNORE INTO work_intents(id,kind,event_id,target,payload,next_at,created_at,updated_at)
 VALUES('room-role-'||lower(hex(randomblob(16))),'records-projection','room-role-'||lower(hex(randomblob(16))),'room',json_object('action','room','room',NEW.room_id),strftime('%s','now'),strftime('%s','now'),strftime('%s','now'));
END;
CREATE TRIGGER IF NOT EXISTS room_member_projection_delete AFTER DELETE ON room_members
BEGIN
 INSERT OR IGNORE INTO work_intents(id,kind,event_id,target,payload,next_at,created_at,updated_at)
 VALUES('room-member-delete-'||lower(hex(randomblob(16))),'records-projection','room-member-delete-'||lower(hex(randomblob(16))),'room',json_object('action','room','room',OLD.room_id,'pubkey',OLD.pubkey,'role',OLD.role,'removed',1),strftime('%s','now'),strftime('%s','now'),strftime('%s','now'));
END;
`

const (
	RoomOpen    = "open"
	RoomMembers = "members"

	RoomRoleOwner  = "owner"
	RoomRoleAdmin  = "admin"
	RoomRoleMember = "member"
)

// Room is a chat room inside the tenant.
type Room struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	About     string `json:"about"`
	Picture   string `json:"picture"`
	Access    string `json:"access"`
	CreatedBy string `json:"created_by"`
	CreatedAt int64  `json:"created_at"`
	EventID   string `json:"event_id"`
	DeletedAt int64  `json:"deleted_at,omitempty"`
}

// RoomMember is one member row of a room.
type RoomMember struct {
	PubKey  string `json:"pubkey"`
	Role    string `json:"role"`
	AddedAt int64  `json:"added_at"`
}

var roomIDRE = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// ValidRoomID reports whether id has the shape of a room id.
func ValidRoomID(id string) bool { return roomIDRE.MatchString(id) }

// RoomAdminKind reports whether kind is a NIP-29 management event that a
// room handles itself when it carries the room's h tag.
func RoomAdminKind(kind int) bool {
	switch kind {
	case event.KIND_PUT_USER, event.KIND_REMOVE_USER, event.KIND_EDIT_METADATA, event.KIND_DELETE_EVENT, event.KIND_CREATE_GROUP, event.KIND_DELETE_GROUP, event.KIND_CREATE_INVITE, event.KIND_PINS, event.KIND_JOIN, event.KIND_LEAVE:
		return true
	}
	return false
}

// RoomNoticeKind reports whether kind is a relay-signed member notice.
func RoomNoticeKind(kind int) bool { return kind == event.KIND_ROOM_MEMBER_ADDED || kind == event.KIND_ROOM_MEMBER_REMOVED }

// RoomAccessFor maps the tenant read rule onto the slug room's access.
func RoomAccessFor(reads string) string {
	if reads == "members" {
		return RoomMembers
	}
	return RoomOpen
}

// EnsureSlugRoom creates or refreshes the room that stands for the tenant's
// own group. Its access follows the tenant read rule and its members mirror
// the tenant roster: the owner is the room owner and moderators are admins.
// The first mirror is silent so an existing roster does not announce every
// member as newly added.
func (s *Service) EnsureSlugRoom(ctx context.Context, slug, access string) error {
	if !ValidRoomID(slug) {
		return fmt.Errorf("invalid: room id %q", slug)
	}
	if access != RoomMembers {
		access = RoomOpen
	}
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO community_meta(key,value) VALUES('rooms.slug',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, slug); err != nil {
			return err
		}
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT id FROM rooms WHERE id=?`, slug).Scan(&existing)
		fresh := errors.Is(err, sql.ErrNoRows)
		if err != nil && !fresh {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO rooms(id,access,created_by,created_at) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET access=excluded.access,deleted_at=0`, slug, access, s.currentOwner(), now()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO room_members(room_id,pubkey,role,added_at) SELECT ?,pubkey,CASE role WHEN 'owner' THEN 'owner' WHEN 'moderator' THEN 'admin' ELSE 'member' END,created_at FROM community_members WHERE true ON CONFLICT(room_id,pubkey) DO UPDATE SET role=excluded.role WHERE role<>excluded.role`, slug); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM room_members WHERE room_id=? AND pubkey NOT IN (SELECT pubkey FROM community_members)`, slug); err != nil {
			return err
		}
		if fresh {
			_, err = tx.ExecContext(ctx, `DELETE FROM work_intents WHERE kind='records-projection' AND target='room' AND state='pending' AND json_extract(payload,'$.room')=? AND json_extract(payload,'$.pubkey') IS NOT NULL`, slug)
		}
		return err
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.slug = slug
	s.mu.Unlock()
	return nil
}

// SlugRoom returns the id of the room that mirrors the tenant roster.
func (s *Service) SlugRoom() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.slug
}

func (s *Service) currentOwner() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.owner
}

// Room returns the room row, including a deleted one, or an empty Room.
func (s *Service) Room(ctx context.Context, id string) (Room, error) {
	return scanRoom(s.store.DB().QueryRowContext(ctx, `SELECT id,name,about,picture,access,created_by,created_at,event_id,deleted_at FROM rooms WHERE id=?`, id))
}

func roomTx(ctx context.Context, tx *sql.Tx, id string) (Room, error) {
	return scanRoom(tx.QueryRowContext(ctx, `SELECT id,name,about,picture,access,created_by,created_at,event_id,deleted_at FROM rooms WHERE id=?`, id))
}

func scanRoom(row *sql.Row) (Room, error) {
	var r Room
	err := row.Scan(&r.ID, &r.Name, &r.About, &r.Picture, &r.Access, &r.CreatedBy, &r.CreatedAt, &r.EventID, &r.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Room{}, nil
	}
	if err != nil {
		return Room{}, fmt.Errorf("room: %w", err)
	}
	return r, nil
}

// Live reports whether the room exists and has not been deleted.
func (r Room) Live() bool { return r.ID != "" && r.DeletedAt == 0 }

// Rooms lists the live rooms ordered by id.
func (s *Service) Rooms(ctx context.Context) ([]Room, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT id,name,about,picture,access,created_by,created_at,event_id,deleted_at FROM rooms WHERE deleted_at=0 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("rooms: %w", err)
	}
	defer rows.Close()
	out := []Room{}
	for rows.Next() {
		var r Room
		if err := rows.Scan(&r.ID, &r.Name, &r.About, &r.Picture, &r.Access, &r.CreatedBy, &r.CreatedAt, &r.EventID, &r.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RoomMembers lists a room's members, owners and admins first.
func (s *Service) RoomMembers(ctx context.Context, id string) ([]RoomMember, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT pubkey,role,added_at FROM room_members WHERE room_id=? ORDER BY CASE role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END,added_at,pubkey`, id)
	if err != nil {
		return nil, fmt.Errorf("room members: %w", err)
	}
	defer rows.Close()
	out := []RoomMember{}
	for rows.Next() {
		var m RoomMember
		if err := rows.Scan(&m.PubKey, &m.Role, &m.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RoomMemberCount counts a room's members.
func (s *Service) RoomMemberCount(ctx context.Context, id string) (int, error) {
	var n int
	err := s.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM room_members WHERE room_id=?`, id).Scan(&n)
	return n, err
}

// RoomRole returns the effective role of pubkey in a room: the stored room
// role, or owner and admin for the tenant owner and moderators, who may
// moderate every room. It is empty for people outside the room.
func (s *Service) RoomRole(ctx context.Context, roomID, pubkey string) (string, error) {
	if pubkey == "" {
		return "", nil
	}
	var role string
	err := s.store.DB().QueryRowContext(ctx, `SELECT role FROM room_members WHERE room_id=? AND pubkey=?`, roomID, pubkey).Scan(&role)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("room role: %w", err)
	}
	if role != "" {
		return role, nil
	}
	tenantRole, err := s.Role(ctx, pubkey)
	if err != nil {
		return "", err
	}
	return impliedRoomRole(tenantRole), nil
}

func impliedRoomRole(tenantRole string) string {
	switch tenantRole {
	case "owner":
		return RoomRoleOwner
	case "moderator":
		return RoomRoleAdmin
	}
	return ""
}

func roomRoleTx(ctx context.Context, tx *sql.Tx, roomID, pubkey string) (string, error) {
	var role string
	err := tx.QueryRowContext(ctx, `SELECT role FROM room_members WHERE room_id=? AND pubkey=?`, roomID, pubkey).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

func (s *Service) effectiveRoomRoleTx(ctx context.Context, tx *sql.Tx, roomID, pubkey string) (string, error) {
	role, err := roomRoleTx(ctx, tx, roomID, pubkey)
	if err != nil || role != "" {
		return role, err
	}
	return impliedRoomRole(s.tenantRoleTx(ctx, tx, pubkey)), nil
}

func (s *Service) tenantRoleTx(ctx context.Context, tx *sql.Tx, pubkey string) string {
	if pubkey == s.currentOwner() {
		return "owner"
	}
	var role string
	_ = tx.QueryRowContext(ctx, `SELECT role FROM community_members WHERE pubkey=?`, pubkey).Scan(&role)
	return role
}

func roomAdmin(role string) bool { return role == RoomRoleOwner || role == RoomRoleAdmin }

// RoomWriteAllowed decides whether pubkey may post to a room. Members always
// may; an open room also accepts tenant members, who join on their first
// message. The second result reports that such a join is pending.
func (s *Service) RoomWriteAllowed(ctx context.Context, roomID, pubkey string) (bool, bool, error) {
	room, err := s.Room(ctx, roomID)
	if err != nil || !room.Live() {
		return false, false, err
	}
	role, err := s.RoomRole(ctx, roomID, pubkey)
	if err != nil {
		return false, false, err
	}
	if role != "" {
		return true, false, nil
	}
	if room.Access != RoomOpen {
		return false, false, nil
	}
	member, err := s.IsMember(ctx, pubkey)
	return member, member, err
}

// RoomVisible decides whether any of the keys may read a room's events.
// Open rooms follow the tenant read rule; members-only rooms are limited to
// their members, the tenant owner and moderators.
func (s *Service) RoomVisible(ctx context.Context, roomID string, keys []string) (bool, error) {
	room, err := s.Room(ctx, roomID)
	if err != nil || !room.Live() {
		return false, err
	}
	if room.Access != RoomMembers {
		return true, nil
	}
	for _, key := range keys {
		role, err := s.RoomRole(ctx, roomID, key)
		if err != nil {
			return false, err
		}
		if role != "" {
			return true, nil
		}
	}
	return false, nil
}

// HandleRoomMessageTx stores a room message and, for an open room, adds a
// tenant member to the room on their first message in the same transaction.
func (s *Service) HandleRoomMessageTx(ctx context.Context, ev event.Event, persist func(*sql.Tx) error) error {
	if persist == nil {
		return errors.New("community: nil event persistence callback")
	}
	roomID := tagValue(ev, "h")
	return s.store.WithTx(ctx, func(tx *sql.Tx) error {
		room, err := roomTx(ctx, tx, roomID)
		if err != nil {
			return err
		}
		if !room.Live() {
			return fmt.Errorf("invalid: unknown room %s", roomID)
		}
		role, err := s.effectiveRoomRoleTx(ctx, tx, roomID, ev.PubKey)
		if err != nil {
			return err
		}
		if role == "" {
			if room.Access != RoomOpen || s.tenantRoleTx(ctx, tx, ev.PubKey) == "" {
				return fmt.Errorf("restricted: not a member of room %s", roomID)
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO room_members(room_id,pubkey,role,added_at) VALUES(?,?,?,?)`, roomID, ev.PubKey, RoomRoleMember, now()); err != nil {
				return err
			}
		}
		return persist(tx)
	})
}

// HandleRoomEventTx applies a NIP-29 management event addressed to a room
// other than the slug room and stores it in the same transaction.
func (s *Service) HandleRoomEventTx(ctx context.Context, ev event.Event, persist func(*sql.Tx) error) (MembershipResult, error) {
	if persist == nil {
		return MembershipResult{}, errors.New("community: nil event persistence callback")
	}
	if !validPubKey(ev.PubKey) {
		return MembershipResult{}, errors.New("invalid: event pubkey")
	}
	roomID := tagValue(ev, "h")
	if !ValidRoomID(roomID) {
		return MembershipResult{}, errors.New("invalid: a room id is 1 to 64 characters of a-z, 0-9, - and _")
	}
	if roomID == s.SlugRoom() {
		return MembershipResult{}, errors.New("invalid: the main room is managed by the relay")
	}
	if !RoomAdminKind(ev.Kind) {
		return MembershipResult{}, fmt.Errorf("unsupported: room event kind %d", ev.Kind)
	}
	if ev.Kind == event.KIND_CREATE_INVITE || ev.Kind == event.KIND_PINS {
		return MembershipResult{}, fmt.Errorf("unsupported: kind %d is not available inside rooms", ev.Kind)
	}
	tenantRole, err := s.Role(ctx, ev.PubKey)
	if err != nil {
		return MembershipResult{}, err
	}
	if tenantRole == "" {
		return MembershipResult{}, errors.New("restricted: rooms are for members of this relay")
	}
	out := MembershipResult{OK: true, Stored: true}
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		room, err := roomTx(ctx, tx, roomID)
		if err != nil {
			return err
		}
		if ev.Kind == event.KIND_CREATE_GROUP {
			return s.createRoomTx(ctx, tx, ev, room, persist)
		}
		if !room.Live() {
			return fmt.Errorf("invalid: unknown room %s", roomID)
		}
		role, err := s.effectiveRoomRoleTx(ctx, tx, roomID, ev.PubKey)
		if err != nil {
			return err
		}
		switch ev.Kind {
		case event.KIND_EDIT_METADATA:
			if !roomAdmin(role) {
				return errors.New("restricted: only room admins edit the room")
			}
			meta := roomMetadata(ev, room)
			if _, err := tx.ExecContext(ctx, `UPDATE rooms SET name=?,about=?,picture=?,access=? WHERE id=?`, meta.Name, meta.About, meta.Picture, meta.Access, roomID); err != nil {
				return err
			}
			if err := enqueueRoomProjectionTx(ctx, tx, ev.ID, roomID, false); err != nil {
				return err
			}
		case event.KIND_PUT_USER:
			if !roomAdmin(role) {
				return errors.New("restricted: only room admins add members")
			}
			target, wanted := roomTarget(ev)
			if !validPubKey(target) {
				return errors.New("invalid: membership event needs a p tag")
			}
			if s.tenantRoleTx(ctx, tx, target) == "" {
				return errors.New("restricted: the person must be a member of this relay first")
			}
			if wanted == RoomRoleOwner && role != RoomRoleOwner {
				return errors.New("restricted: only a room owner appoints owners")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO room_members(room_id,pubkey,role,added_at) VALUES(?,?,?,?) ON CONFLICT(room_id,pubkey) DO UPDATE SET role=excluded.role WHERE role<>excluded.role`, roomID, target, wanted, now()); err != nil {
				return err
			}
		case event.KIND_REMOVE_USER:
			if !roomAdmin(role) {
				return errors.New("restricted: only room admins remove members")
			}
			target, _ := roomTarget(ev)
			if !validPubKey(target) {
				return errors.New("invalid: membership event needs a p tag")
			}
			current, err := roomRoleTx(ctx, tx, roomID, target)
			if err != nil {
				return err
			}
			if current == "" {
				out = MembershipResult{OK: true, Message: "duplicate: not a member", Stored: false}
				return nil
			}
			if current == RoomRoleOwner {
				if role != RoomRoleOwner {
					return errors.New("restricted: only a room owner removes an owner")
				}
				if err := guardLastOwnerTx(ctx, tx, roomID); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM room_members WHERE room_id=? AND pubkey=?`, roomID, target); err != nil {
				return err
			}
		case event.KIND_JOIN:
			current, err := roomRoleTx(ctx, tx, roomID, ev.PubKey)
			if err != nil {
				return err
			}
			if current != "" {
				out = MembershipResult{OK: true, Message: "duplicate: already a member", Stored: false}
				return nil
			}
			if room.Access != RoomOpen {
				return errors.New("restricted: this room is members-only; ask a room admin to add you")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO room_members(room_id,pubkey,role,added_at) VALUES(?,?,?,?)`, roomID, ev.PubKey, RoomRoleMember, now()); err != nil {
				return err
			}
		case event.KIND_LEAVE:
			current, err := roomRoleTx(ctx, tx, roomID, ev.PubKey)
			if err != nil {
				return err
			}
			if current == "" {
				return errors.New("invalid: not a member of this room")
			}
			if current == RoomRoleOwner {
				if err := guardLastOwnerTx(ctx, tx, roomID); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM room_members WHERE room_id=? AND pubkey=?`, roomID, ev.PubKey); err != nil {
				return err
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
				var author string
				err := tx.QueryRowContext(ctx, `SELECT pubkey FROM events WHERE id=? AND EXISTS(SELECT 1 FROM tags WHERE event_id=events.id AND name='h' AND value=?)`, id, roomID).Scan(&author)
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
				}
				if !roomAdmin(role) && author != ev.PubKey {
					return errors.New("restricted: only room admins delete other people's messages")
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE id=?`, id); err != nil {
					return err
				}
			}
		case event.KIND_DELETE_GROUP:
			if role != RoomRoleOwner {
				return errors.New("restricted: only a room owner deletes the room")
			}
			if _, err := tx.ExecContext(ctx, `UPDATE rooms SET deleted_at=? WHERE id=?`, now(), roomID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM room_members WHERE room_id=?`, roomID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE id IN (SELECT event_id FROM tags WHERE name='h' AND value=?)`, roomID); err != nil {
				return err
			}
			if err := enqueueRoomProjectionTx(ctx, tx, ev.ID, roomID, true); err != nil {
				return err
			}
		}
		if err := s.recordTx(ctx, tx, ev.PubKey, roomAction(ev.Kind), roomID, strings.TrimSpace(tagValue(ev, "p"))); err != nil {
			return err
		}
		return persist(tx)
	})
	return out, err
}

func (s *Service) createRoomTx(ctx context.Context, tx *sql.Tx, ev event.Event, room Room, persist func(*sql.Tx) error) error {
	if room.Live() {
		return errors.New("duplicate: a room with this id exists")
	}
	limit := s.currentPolicy().Rooms
	if limit <= 0 {
		return errors.New("restricted: room creation is switched off on this relay")
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rooms WHERE deleted_at=0 AND id<>?`, s.SlugRoom()).Scan(&count); err != nil {
		return err
	}
	if count >= limit {
		return fmt.Errorf("restricted: this relay allows %d rooms", limit)
	}
	meta := roomMetadata(ev, Room{ID: tagValue(ev, "h"), Access: RoomOpen})
	if meta.Name == "" {
		meta.Name = meta.ID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO rooms(id,name,about,picture,access,created_by,created_at,event_id,deleted_at) VALUES(?,?,?,?,?,?,?,?,0) ON CONFLICT(id) DO UPDATE SET name=excluded.name,about=excluded.about,picture=excluded.picture,access=excluded.access,created_by=excluded.created_by,created_at=excluded.created_at,event_id=excluded.event_id,deleted_at=0`, meta.ID, meta.Name, meta.About, meta.Picture, meta.Access, ev.PubKey, now(), ev.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM room_members WHERE room_id=?`, meta.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO room_members(room_id,pubkey,role,added_at) VALUES(?,?,?,?)`, meta.ID, ev.PubKey, RoomRoleOwner, now()); err != nil {
		return err
	}
	if err := s.recordTx(ctx, tx, ev.PubKey, "create-room", meta.ID, meta.Access); err != nil {
		return err
	}
	return persist(tx)
}

func guardLastOwnerTx(ctx context.Context, tx *sql.Tx, roomID string) error {
	var owners int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM room_members WHERE room_id=? AND role='owner'`, roomID).Scan(&owners); err != nil {
		return err
	}
	if owners <= 1 {
		return errors.New("restricted: a room keeps at least one owner; appoint another owner first")
	}
	return nil
}

func enqueueRoomProjectionTx(ctx context.Context, tx *sql.Tx, eventID, roomID string, deleted bool) error {
	payload := fmt.Sprintf(`{"action":"room","room":%q}`, roomID)
	if deleted {
		payload = fmt.Sprintf(`{"action":"room","room":%q,"deleted":1}`, roomID)
	}
	id := "room-" + roomID + "-" + eventID
	stamp := now()
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO work_intents(id,kind,event_id,target,payload,next_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, id, "records-projection", id, "room", payload, stamp, stamp, stamp)
	return err
}

// roomTarget returns the p tag target and the requested room role, which
// defaults to member.
func roomTarget(ev event.Event) (string, string) {
	for _, tag := range ev.Tags {
		if len(tag) > 1 && tag[0] == "p" {
			role := RoomRoleMember
			if len(tag) > 2 {
				switch strings.ToLower(strings.TrimSpace(tag[2])) {
				case RoomRoleOwner:
					role = RoomRoleOwner
				case RoomRoleAdmin, "moderator":
					role = RoomRoleAdmin
				}
			}
			return tag[1], role
		}
	}
	return "", RoomRoleMember
}

// roomMetadata applies the metadata tags of a 9007 or 9002 on top of the
// current room. Buzz's channel_type and NIP-29's visibility markers both
// select the access rule.
func roomMetadata(ev event.Event, current Room) Room {
	next := current
	for _, tag := range ev.Tags {
		if len(tag) == 0 {
			continue
		}
		value := ""
		if len(tag) > 1 {
			value = strings.TrimSpace(tag[1])
		}
		switch tag[0] {
		case "name":
			next.Name = value[:min(200, len(value))]
		case "about":
			next.About = value[:min(2000, len(value))]
		case "picture":
			next.Picture = value[:min(2000, len(value))]
		case "visibility", "channel_type", "access":
			if access, ok := roomAccessValue(value); ok {
				next.Access = access
			}
		case "open", "public":
			if len(tag) == 1 {
				next.Access = RoomOpen
			}
		case "closed", "private":
			if len(tag) == 1 {
				next.Access = RoomMembers
			}
		}
	}
	if next.Access != RoomMembers {
		next.Access = RoomOpen
	}
	return next
}

func roomAccessValue(value string) (string, bool) {
	switch strings.ToLower(value) {
	case "open", "public":
		return RoomOpen, true
	case "members", "members-only", "private", "closed", "invite":
		return RoomMembers, true
	}
	return "", false
}

func roomAction(kind int) string {
	switch kind {
	case event.KIND_EDIT_METADATA:
		return "edit-room"
	case event.KIND_PUT_USER:
		return "room-put-user"
	case event.KIND_REMOVE_USER:
		return "room-remove-user"
	case event.KIND_JOIN:
		return "room-join"
	case event.KIND_LEAVE:
		return "room-leave"
	case event.KIND_DELETE_EVENT:
		return "room-delete-event"
	case event.KIND_DELETE_GROUP:
		return "delete-room"
	}
	return "room"
}

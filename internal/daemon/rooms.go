package daemon

import (
	"context"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// roomMessageKinds are the events a room timeline shows: chat, threads and
// their replies, reactions, Buzz rich content and edits, and the relay's
// member notices.
var roomMessageKinds = []int{event.KIND_CHAT, event.KIND_THREAD, event.KIND_THREAD_REPLY, 7, event.KIND_RICH_CONTENT, event.KIND_CONTENT_EDIT, event.KIND_ROOM_MEMBER_ADDED, event.KIND_ROOM_MEMBER_REMOVED}

// projectRoom refreshes the relay's records for a room after a durable
// change. The slug room's 39000 to 39002 records come from the tenant
// projection; here it only receives member notices. A room that has been
// deleted since the change was queued has its records retired instead.
func (t *Tenant) projectRoom(ctx context.Context, roomID, pubkey string, removed, deleted bool) error {
	now := time.Now().Unix()
	room, err := t.community.Room(ctx, roomID)
	if err != nil {
		return err
	}
	if deleted || !room.Live() {
		return t.records.RetireRoom(ctx, roomID)
	}
	if roomID != t.meta.Name {
		if _, err := t.records.PublishRoom(ctx, roomID, now); err != nil {
			return err
		}
	}
	if pubkey == "" {
		return nil
	}
	_, err = t.records.RoomNotice(ctx, roomID, pubkey, !removed, now)
	return err
}

// roomFor loads a live room the actor may see, or explains why not.
func (t *Tenant) roomFor(ctx context.Context, actor, id string) (community.Room, string, error) {
	if !community.ValidRoomID(id) {
		return community.Room{}, "", errRoomNotFound
	}
	room, err := t.community.Room(ctx, id)
	if err != nil {
		return community.Room{}, "", err
	}
	if !room.Live() {
		return community.Room{}, "", errRoomNotFound
	}
	role, err := t.community.RoomRole(ctx, id, actor)
	if err != nil {
		return community.Room{}, "", err
	}
	if room.Access == community.RoomMembers && role == "" {
		if actor == "" {
			return community.Room{}, "", errRoomAuth
		}
		return community.Room{}, "", errRoomRestricted
	}
	return room, role, nil
}

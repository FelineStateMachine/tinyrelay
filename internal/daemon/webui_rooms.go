package daemon

import (
	"context"
	"errors"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/webui"
)

var _ webui.RoomsReader = (*Tenant)(nil)
var _ webui.RoomsReader = backend{}

func (b backend) ListRooms(ctx context.Context, actor, cursor string, limit int) (webui.RoomList, error) {
	return b.tenant.ListRooms(ctx, actor, cursor, limit)
}

func (b backend) ReadRoom(ctx context.Context, actor, roomID, cursor string, limit int) (webui.RoomPage, error) {
	return b.tenant.ReadRoom(ctx, actor, roomID, cursor, limit)
}

func (b backend) ReadThread(ctx context.Context, actor, roomID, rootID, cursor string, limit int) (webui.RoomPage, error) {
	return b.tenant.ReadThread(ctx, actor, roomID, rootID, cursor, limit)
}

func (t *Tenant) ListRooms(ctx context.Context, actor, cursor string, limit int) (webui.RoomList, error) {
	ctx, done, err := t.beginOperation(ctx)
	if err != nil {
		return webui.RoomList{}, err
	}
	defer done()
	return t.listRoomsCore(ctx, actor, cursor, limit)
}

func (t *Tenant) listRoomsCore(ctx context.Context, actor, cursor string, limit int) (webui.RoomList, error) {
	if err := t.browseRead(ctx, actor); err != nil {
		return webui.RoomList{}, err
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rooms, err := t.community.Rooms(ctx)
	if err != nil {
		return webui.RoomList{}, err
	}
	result := webui.RoomList{}
	for _, room := range rooms {
		if room.ID <= cursor {
			continue
		}
		_, role, roleErr := t.roomFor(ctx, actor, room.ID)
		if roleErr != nil {
			continue
		}
		if len(result.Rooms) == limit {
			result.NextCursor = result.Rooms[len(result.Rooms)-1].ID
			break
		}
		summary, summaryErr := t.roomSummary(ctx, actor, room, role)
		if summaryErr != nil {
			return webui.RoomList{}, summaryErr
		}
		result.Rooms = append(result.Rooms, webuiRoomSummary(summary))
	}
	return result, nil
}

func (t *Tenant) ReadRoom(ctx context.Context, actor, roomID, cursor string, limit int) (webui.RoomPage, error) {
	ctx, done, err := t.beginOperation(ctx)
	if err != nil {
		return webui.RoomPage{}, err
	}
	defer done()
	return t.readRoomCore(ctx, actor, roomID, cursor, limit)
}

func (t *Tenant) readRoomCore(ctx context.Context, actor, roomID, cursor string, limit int) (webui.RoomPage, error) {
	if err := t.browseRead(ctx, actor); err != nil {
		return webui.RoomPage{}, err
	}
	room, role, err := t.roomFor(ctx, actor, roomID)
	if err != nil {
		return webui.RoomPage{}, err
	}
	position, err := parseCollaborationCursor(cursor)
	if err != nil {
		return webui.RoomPage{}, errors.New("invalid: room cursor")
	}
	summary, err := t.roomSummary(ctx, actor, room, role)
	if err != nil {
		return webui.RoomPage{}, err
	}
	members, err := t.webuiRoomMembers(ctx, room.ID, role)
	if err != nil {
		return webui.RoomPage{}, err
	}
	messages, next, err := t.roomMessages(ctx, actor, event.Filter{Kinds: roomMessageKinds, Tags: map[string][]string{"h": {room.ID}}}, position, normalizeRoomLimit(limit))
	if err != nil {
		return webui.RoomPage{}, err
	}
	edits, err := t.roomEdits(ctx, actor, room.ID, messages)
	if err != nil {
		return webui.RoomPage{}, err
	}
	return webui.RoomPage{Room: webuiRoomSummary(summary), Members: members, Messages: messages, Edits: edits, NextCursor: next}, nil
}

func (t *Tenant) ReadThread(ctx context.Context, actor, roomID, rootID, cursor string, limit int) (webui.RoomPage, error) {
	ctx, done, err := t.beginOperation(ctx)
	if err != nil {
		return webui.RoomPage{}, err
	}
	defer done()
	return t.readThreadCore(ctx, actor, roomID, rootID, cursor, limit)
}

func (t *Tenant) readThreadCore(ctx context.Context, actor, roomID, rootID, cursor string, limit int) (webui.RoomPage, error) {
	if err := t.browseRead(ctx, actor); err != nil {
		return webui.RoomPage{}, err
	}
	room, role, err := t.roomFor(ctx, actor, roomID)
	if err != nil {
		return webui.RoomPage{}, err
	}
	if len(rootID) != 64 || !hexLower(rootID) {
		return webui.RoomPage{}, errors.New("invalid: thread root")
	}
	position, err := parseCollaborationCursor(cursor)
	if err != nil {
		return webui.RoomPage{}, errors.New("invalid: thread cursor")
	}
	root, err := t.roomThreadRoot(ctx, actor, room.ID, rootID)
	if err != nil {
		return webui.RoomPage{}, err
	}
	all, err := t.roomThreadReplies(ctx, actor, room.ID, root.ID)
	if err != nil {
		return webui.RoomPage{}, err
	}
	replies, next := roomThreadPage(all, position, normalizeRoomLimit(limit))
	targets := append([]event.Event{root}, replies...)
	edits, err := t.roomEdits(ctx, actor, room.ID, targets)
	if err != nil {
		return webui.RoomPage{}, err
	}
	members, err := t.webuiRoomMembers(ctx, room.ID, role)
	if err != nil {
		return webui.RoomPage{}, err
	}
	return webui.RoomPage{Room: webuiRoomSummaryFromRoom(room), Members: members, Root: &root, Replies: replies, Edits: edits, NextCursor: next}, nil
}

func (t *Tenant) webuiRoomMembers(ctx context.Context, roomID, role string) ([]webui.RoomMember, error) {
	if role == "" && !t.Policy().DirectoryPublic {
		return nil, nil
	}
	rows, err := t.community.RoomMembers(ctx, roomID)
	if err != nil {
		return nil, err
	}
	members := make([]webui.RoomMember, 0, len(rows))
	for _, row := range rows {
		tenantRole, err := t.community.Role(ctx, row.PubKey)
		if err != nil {
			return nil, err
		}
		member := webui.RoomMember{PubKey: row.PubKey, Role: row.Role, AddedAt: row.AddedAt, Agent: tenantRole == "agent"}
		if member.Agent {
			grant, found, grantErr := t.community.AgentGrant(ctx, row.PubKey)
			if grantErr != nil {
				return nil, grantErr
			}
			if found {
				member.Operator = grant.Owner
			}
		}
		members = append(members, member)
	}
	return members, nil
}

func webuiRoomSummary(summary roomSummary) webui.RoomSummary {
	room := webuiRoomSummaryFromRoom(summary.Room)
	room.Members, room.LastMessageAt, room.Role = summary.Members, summary.LastMessageAt, summary.Role
	return room
}

func webuiRoomSummaryFromRoom(room community.Room) webui.RoomSummary {
	return webui.RoomSummary{ID: room.ID, Name: room.Name, About: room.About, Picture: room.Picture, Access: room.Access, CreatedBy: room.CreatedBy, CreatedAt: room.CreatedAt, EventID: room.EventID, DeletedAt: room.DeletedAt}
}

func normalizeRoomLimit(limit int) int {
	if limit <= 0 || limit > 100 {
		return 100
	}
	return limit
}

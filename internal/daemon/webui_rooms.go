package daemon

import (
	"context"
	"errors"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
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
	return t.readRoomCore(ctx, actor, roomID, cursor, limit, true)
}

func (t *Tenant) readRoomCore(ctx context.Context, actor, roomID, cursor string, limit int, foldReplies bool) (webui.RoomPage, error) {
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
	readMessages := t.roomMessages
	if foldReplies {
		readMessages = t.roomTimelineMessages
	}
	messageFilter := event.Filter{Kinds: roomMessageKinds, Tags: map[string][]string{"h": {room.ID}}}
	messages, next, err := readMessages(ctx, actor, messageFilter, position, normalizeRoomLimit(limit))
	if err != nil {
		return webui.RoomPage{}, err
	}
	edits, err := t.roomEdits(ctx, actor, room.ID, messages)
	if err != nil {
		return webui.RoomPage{}, err
	}
	page := webui.RoomPage{Room: webuiRoomSummary(summary), Members: members, Messages: messages, Edits: edits, NextCursor: next}
	if foldReplies {
		page.ReplyCounts, page.ReactionCounts, err = t.roomInteractionSummaries(ctx, actor, room.ID, messages)
		if err != nil {
			return webui.RoomPage{}, err
		}
	}
	return page, nil
}

// roomTimelineMessages pages by visible top-level messages rather than raw
// events. Folded replies and reactions are summarized separately so a burst
// of replies cannot make the page empty or consume unbounded response memory.
func (t *Tenant) roomTimelineMessages(ctx context.Context, actor string, filter event.Filter, cursor *storage.EventCursor, limit int) ([]event.Event, string, error) {
	if limit <= 0 {
		limit = 100
	}
	position := cursor
	rows := make([]event.Event, 0, limit)
	for {
		page, next, err := t.roomMessages(ctx, actor, filter, position, 100)
		if err != nil {
			return nil, "", err
		}
		for i, row := range page {
			if roomTimelineTopLevel(row) || (row.Kind == event.KIND_ROOM_MEMBER_ADDED || row.Kind == event.KIND_ROOM_MEMBER_REMOVED) && event.Tag(row, "p") != "" {
				rows = append(rows, row)
			}
			if len(rows) < limit {
				continue
			}
			if next == "" && i == len(page)-1 {
				return rows, "", nil
			}
			return rows, collaborationCursor(collaborationItemFrom(row)), nil
		}
		if next == "" {
			return rows, "", nil
		}
		position, err = parseCollaborationCursor(next)
		if err != nil {
			return nil, "", err
		}
	}
}

func (t *Tenant) roomInteractionSummaries(ctx context.Context, actor, roomID string, roots []event.Event) (map[string]int, map[string]map[string]int, error) {
	counts := make(map[string]int)
	reactions := make(map[string]map[string]int)
	for _, root := range roots {
		if !roomTimelineTopLevel(root) {
			continue
		}
		replies, err := t.roomThreadReplies(ctx, actor, roomID, root.ID)
		if errors.Is(err, errRoomThreadSizeLimit) || errors.Is(err, errRoomThreadDepthLimit) {
			// A bounded thread view must not prevent reading the rest of the
			// room. Omit its count instead of presenting a partial total.
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		counts[root.ID] = len(replies)
	}
	ids := make([]string, 0, len(roots))
	selected := make(map[string]bool, len(roots))
	for _, root := range roots {
		if roomTimelineTopLevel(root) {
			ids = append(ids, root.ID)
			selected[root.ID] = true
		}
	}
	if len(ids) == 0 {
		return counts, reactions, nil
	}
	filter := event.Filter{Kinds: []int{7}, Tags: map[string][]string{"h": {roomID}, "e": ids}}
	var cursor *storage.EventCursor
	for {
		rows, next, err := t.roomMessages(ctx, actor, filter, cursor, 100)
		if err != nil {
			return nil, nil, err
		}
		for _, row := range rows {
			targets := event.TagValues(row, "e")
			if len(targets) == 0 {
				continue
			}
			target := targets[len(targets)-1]
			if !selected[target] {
				continue
			}
			content := strings.TrimSpace(row.Content)
			if content == "" || content == "+" {
				content = "+1"
			}
			if reactions[target] == nil {
				reactions[target] = make(map[string]int)
			}
			reactions[target][content]++
		}
		if next == "" {
			return counts, reactions, nil
		}
		cursor, err = parseCollaborationCursor(next)
		if err != nil {
			return nil, nil, err
		}
	}
}

func roomTimelineTopLevel(row event.Event) bool {
	switch row.Kind {
	case event.KIND_CHAT, event.KIND_THREAD, event.KIND_RICH_CONTENT:
		return event.RoomReplyRoot(row) == ""
	default:
		return false
	}
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
	return webui.RoomPage{Room: webuiRoomSummaryFromRoom(room), Members: members, Root: &root, Replies: replies, ReplyCounts: map[string]int{root.ID: len(all)}, Edits: edits, NextCursor: next}, nil
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

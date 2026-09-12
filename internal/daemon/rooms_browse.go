package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/webui"
)

var (
	errRoomNotFound   = errors.New("not found: room")
	errRoomAuth       = errors.New("auth-required: this room is members-only")
	errRoomRestricted = errors.New("restricted: this room is members-only")
)

type roomBrowseRequest struct {
	ID     string `json:"id"`
	Event  string `json:"event"`
	Cursor string `json:"cursor"`
	Limit  int    `json:"limit"`
}

type roomSummary struct {
	community.Room
	Members       int    `json:"members"`
	LastMessageAt int64  `json:"last_message_at"`
	Role          string `json:"role"`
}

// roomMemberView is a room member with the relay-level marker a page needs:
// whether the member is an agent identity.
type roomMemberView struct {
	community.RoomMember
	Agent bool `json:"agent,omitempty"`
}

func roomBrowseMethod(method string) bool {
	return method == "browserooms" || method == "browseroom" || method == "browsethread"
}

// executeRoomBrowse serves the room reads used by the web pages and agents.
// Access follows the tenant read rule and then the room's own access.
func (t *Tenant) executeRoomBrowse(ctx context.Context, actor, method string, params []json.RawMessage) (result any, err error) {
	ctx, finish := t.app.telemetry.Start(ctx, "rooms")
	defer func() {
		if err != nil {
			finish("error")
		} else {
			finish("success")
		}
	}()
	if err := t.browseRead(ctx, actor); err != nil {
		return nil, err
	}
	q := roomBrowseRequest{}
	if len(params) > 0 {
		if err := json.Unmarshal(params[0], &q); err != nil {
			return nil, fmt.Errorf("invalid: room parameters: %w", err)
		}
	}
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 100
	}
	switch method {
	case "browserooms":
		return t.browseRooms(ctx, actor, q)
	case "browseroom":
		return t.browseRoom(ctx, actor, q)
	case "browsethread":
		return t.browseThread(ctx, actor, q)
	}
	return nil, errors.New("unsupported: room operation")
}

func (t *Tenant) browseRooms(ctx context.Context, actor string, q roomBrowseRequest) (any, error) {
	list, err := t.listRoomsCore(ctx, actor, q.Cursor, q.Limit)
	if err != nil {
		return nil, err
	}
	items := make([]roomSummary, 0, len(list.Rooms))
	for _, summary := range list.Rooms {
		items = append(items, roomSummaryFromWebUI(summary))
	}
	return map[string]any{"items": items, "next_cursor": list.NextCursor}, nil
}

func (t *Tenant) roomSummary(ctx context.Context, actor string, room community.Room, role string) (roomSummary, error) {
	count, err := t.community.RoomMemberCount(ctx, room.ID)
	if err != nil {
		return roomSummary{}, err
	}
	summary := roomSummary{Room: room, Members: count, Role: role}
	latest, err := t.store.Query(ctx, event.Filter{Kinds: []int{event.KIND_CHAT, event.KIND_THREAD, event.KIND_THREAD_REPLY, event.KIND_RICH_CONTENT}, Tags: map[string][]string{"h": {room.ID}}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{PubKeys: browseSession(t, actor).PubKeys}, Limit: 1})
	if err != nil {
		return roomSummary{}, err
	}
	if len(latest.Events) > 0 {
		summary.LastMessageAt = latest.Events[0].CreatedAt
	}
	return summary, nil
}

func roomSummaryFromWebUI(summary webui.RoomSummary) roomSummary {
	return roomSummary{Room: community.Room{ID: summary.ID, Name: summary.Name, About: summary.About, Picture: summary.Picture, Access: summary.Access, CreatedBy: summary.CreatedBy, CreatedAt: summary.CreatedAt, EventID: summary.EventID, DeletedAt: summary.DeletedAt}, Members: summary.Members, LastMessageAt: summary.LastMessageAt, Role: summary.Role}
}

func wireRoomPage(page webui.RoomPage, thread bool) map[string]any {
	room := roomSummaryFromWebUI(page.Room)
	members := make([]roomMemberView, 0, len(page.Members))
	for _, member := range page.Members {
		members = append(members, roomMemberView{RoomMember: community.RoomMember{PubKey: member.PubKey, Role: member.Role, AddedAt: member.AddedAt}, Agent: member.Agent})
	}
	if thread {
		return map[string]any{"room": room, "root": page.Root, "replies": page.Replies, "edits": page.Edits, "next_cursor": page.NextCursor}
	}
	return map[string]any{"room": room, "members": members, "messages": page.Messages, "edits": page.Edits, "next_cursor": page.NextCursor}
}

func (t *Tenant) browseRoom(ctx context.Context, actor string, q roomBrowseRequest) (any, error) {
	// The MCP browser contract exposes raw room events for agents. The typed
	// web reader folds replies into top-level pages for the UI.
	page, err := t.readRoomCore(ctx, actor, q.ID, q.Cursor, q.Limit, false)
	if err != nil {
		return nil, err
	}
	return wireRoomPage(page, false), nil
}

func (t *Tenant) browseThread(ctx context.Context, actor string, q roomBrowseRequest) (any, error) {
	page, err := t.readThreadCore(ctx, actor, q.ID, q.Event, q.Cursor, q.Limit)
	if err != nil {
		return nil, err
	}
	return wireRoomPage(page, true), nil
}

// roomMessages pages a room filter newest first through the read gate. The
// cursor is the created_at and id of the last event on the previous page.
func (t *Tenant) roomMessages(ctx context.Context, actor string, filter event.Filter, cursor *storage.EventCursor, limit int) ([]event.Event, string, error) {
	session := browseSession(t, actor)
	if _, err := t.gate.Read(ctx, []event.Filter{filter}, session); err != nil {
		return nil, "", err
	}
	page, err := t.store.Query(ctx, filter, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{PubKeys: session.PubKeys}, Limit: limit, Before: cursor})
	if err != nil {
		return nil, "", err
	}
	rows := make([]event.Event, 0, len(page.Events))
	for _, row := range page.Events {
		if t.gate.CanSee(ctx, row, session, nil) {
			rows = append(rows, row)
		}
	}
	sortEventsNewestFirst(rows)
	next := ""
	if page.More && len(page.Events) > 0 {
		last := page.Events[len(page.Events)-1]
		next = collaborationCursor(collaborationItemFrom(last))
	}
	return rows, next, nil
}

// roomEdits returns the newest author-owned edit for each visible message.
// It deliberately queries independently of the timeline cursor: an edit is
// a replacement for its target and may have been published after the page
// containing that target was read. A thread page has at most 100 replies
// plus its root; each target contributes at most one replacement.
func (t *Tenant) roomEdits(ctx context.Context, actor, roomID string, targets []event.Event) ([]event.Event, error) {
	if len(targets) == 0 {
		return []event.Event{}, nil
	}
	validTargets := make([]event.Event, 0, len(targets))
	for _, target := range targets {
		if len(target.ID) != 64 || !hexLower(target.ID) || target.PubKey == "" ||
			(target.Kind != event.KIND_CHAT && target.Kind != event.KIND_THREAD && target.Kind != event.KIND_THREAD_REPLY && target.Kind != event.KIND_RICH_CONTENT) {
			continue
		}
		validTargets = append(validTargets, target)
		if len(validTargets) == 101 {
			break
		}
	}
	if len(validTargets) == 0 {
		return []event.Event{}, nil
	}
	newest := make(map[string]event.Event, len(validTargets))
	for _, target := range validTargets {
		since := target.CreatedAt
		filter := event.Filter{Kinds: []int{event.KIND_CONTENT_EDIT}, Authors: []string{target.PubKey}, Tags: map[string][]string{"h": {roomID}, "e": {target.ID}}, Since: &since}
		var cursor *storage.EventCursor
		for {
			edits, next, err := t.roomMessages(ctx, actor, filter, cursor, 100)
			if err != nil {
				return nil, err
			}
			for _, edit := range edits {
				if lastRoomEditTarget(edit) == target.ID {
					newest[target.ID] = edit
					break
				}
			}
			if _, found := newest[target.ID]; found || next == "" {
				break
			}
			cursor, err = parseCollaborationCursor(next)
			if err != nil {
				return nil, err
			}
		}
	}
	result := make([]event.Event, 0, len(newest))
	for _, edit := range newest {
		result = append(result, edit)
	}
	sortEventsNewestFirst(result)
	return result, nil
}

func lastRoomEditTarget(e event.Event) string {
	target := ""
	for _, tag := range e.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			target = tag[1]
		}
	}
	return target
}

package webui

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// RoomsReader is the presentation-owned read contract for room pages. The
// legacy Backend.Query method remains supported for adapters that have not
// adopted this interface yet.
type RoomsReader interface {
	ListRooms(context.Context, string, string, int) (RoomList, error)
	ReadRoom(context.Context, string, string, string, int) (RoomPage, error)
	ReadThread(context.Context, string, string, string, string, int) (RoomPage, error)
}

type RoomList struct {
	Rooms      []RoomSummary `json:"rooms"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

type RoomSummary struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	Access        string `json:"access,omitempty"`
	About         string `json:"about,omitempty"`
	Picture       string `json:"picture,omitempty"`
	CreatedBy     string `json:"created_by,omitempty"`
	CreatedAt     int64  `json:"created_at,omitempty"`
	EventID       string `json:"event_id,omitempty"`
	DeletedAt     int64  `json:"deleted_at,omitempty"`
	Members       int    `json:"members,omitempty"`
	LastMessageAt int64  `json:"last_message_at,omitempty"`
	Role          string `json:"role,omitempty"`
}

type RoomMember struct {
	PubKey  string `json:"pubkey"`
	Role    string `json:"role,omitempty"`
	Agent   bool   `json:"agent,omitempty"`
	AddedAt int64  `json:"added_at,omitempty"`
}

type RoomMessage = event.Event

type RoomPage struct {
	Room       RoomSummary   `json:"room"`
	Members    []RoomMember  `json:"members,omitempty"`
	Messages   []RoomMessage `json:"messages,omitempty"`
	Replies    []RoomMessage `json:"replies,omitempty"`
	Edits      []RoomMessage `json:"edits,omitempty"`
	Root       *RoomMessage  `json:"root,omitempty"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// rowsReader adapts the legacy method-and-JSON backend to the read shapes
// used by page renderers. It keeps wire compatibility while making list
// queries consistently return rows at the presentation boundary.
type rowsReader struct{ backend Backend }

func (r rowsReader) rows(ctx context.Context, method string, params []json.RawMessage, actor string) ([]any, error) {
	result, err := r.backend.Query(ctx, method, params, actor)
	if err != nil {
		return nil, err
	}
	return browseRows(result), nil
}

func (a *App) readRows(ctx context.Context, method string, params []json.RawMessage, actor string) ([]any, error) {
	return rowsReader{backend: a.backend}.rows(ctx, method, params, actor)
}

func (a *App) roomsReader() RoomsReader {
	reader, _ := a.backend.(RoomsReader)
	return reader
}

func roomListValue(list RoomList) any {
	return map[string]any{"items": list.Rooms, "next_cursor": list.NextCursor}
}

func roomPageValueFromContract(page RoomPage) any {
	return map[string]any{
		"room":        page.Room,
		"members":     page.Members,
		"messages":    page.Messages,
		"replies":     page.Replies,
		"edits":       page.Edits,
		"root":        page.Root,
		"next_cursor": page.NextCursor,
	}
}

func (a *App) typedRoomResult(ctx context.Context, route roomPath, query url.Values, actor string) (any, error, bool) {
	reader := a.roomsReader()
	if reader == nil || route.tab == "" {
		return nil, nil, false
	}
	var (
		value any
		err   error
	)
	switch route.tab {
	case "rooms":
		list, readErr := reader.ListRooms(ctx, actor, query.Get("cursor"), 100)
		value, err = roomListValue(list), readErr
	case "room":
		page, readErr := reader.ReadRoom(ctx, actor, route.id, query.Get("cursor"), 50)
		value, err = roomPageValueFromContract(page), readErr
	case "thread":
		page, readErr := reader.ReadThread(ctx, actor, route.id, route.event, query.Get("cursor"), 50)
		value, err = roomPageValueFromContract(page), readErr
	}
	return value, err, true
}

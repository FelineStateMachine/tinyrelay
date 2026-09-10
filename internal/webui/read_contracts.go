package webui

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// RoomsReader supplies the typed reads used by room pages. actor is the
// authenticated public key, or an empty string for an anonymous request.
// cursor is empty for the newest page and otherwise is the value returned in
// NextCursor. limit requests at most that many rows. The daemon adapter uses
// 100 when the requested limit falls outside 1 through 100.
//
// ListRooms returns visible room summaries ordered by room ID and a cursor for
// the next page. ReadRoom returns the room summary, visible members, newest
// room messages, message edits and a cursor for older messages. ReadThread
// returns the room summary, one root event, its replies, edits and a cursor for
// older replies. Reads enforce tenant and room access and return errors for
// missing rooms, invalid cursors or an inaccessible room. ReadThread also
// returns an error when rootID is not a lower-case hexadecimal event ID or does
// not identify a message in roomID.
type RoomsReader interface {
	// ListRooms returns up to limit visible room summaries after cursor.
	ListRooms(ctx context.Context, actor, cursor string, limit int) (RoomList, error)
	// ReadRoom returns one visible room, its members, messages and edits after cursor.
	ReadRoom(ctx context.Context, actor, roomID, cursor string, limit int) (RoomPage, error)
	// ReadThread returns one room thread root, replies and edits after cursor.
	ReadThread(ctx context.Context, actor, roomID, rootID, cursor string, limit int) (RoomPage, error)
}

type RoomList struct {
	// Rooms contains visible room summaries in ascending ID order.
	Rooms []RoomSummary `json:"rooms"`
	// NextCursor is the last room ID when another page is available.
	NextCursor string `json:"next_cursor,omitempty"`
}

// RoomSummary is the room metadata shown in the rail and room header.
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

// RoomPage is the typed result for a room or thread read. A room read fills
// Members and Messages; a thread read fills Root and Replies. Edits contains
// the newest visible edit for the returned targets.
type RoomPage struct {
	Room       RoomSummary   `json:"room"`
	Members    []RoomMember  `json:"members,omitempty"`
	Messages   []RoomMessage `json:"messages,omitempty"`
	Replies    []RoomMessage `json:"replies,omitempty"`
	Edits      []RoomMessage `json:"edits,omitempty"`
	Root       *RoomMessage  `json:"root,omitempty"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// rowsReader converts method-and-JSON query results into the row slices used
// by page renderers.
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

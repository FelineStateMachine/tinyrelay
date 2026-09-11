package daemon

import (
	"context"
	"errors"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func (t *Tenant) roomThreadRoot(ctx context.Context, actor, room, id string) (event.Event, error) {
	seen := map[string]bool{}
	for len(seen) < 64 && !seen[id] {
		seen[id] = true
		rows, err := t.Query(ctx, []event.Filter{{IDs: []string{id}, Limit: intPtr(1)}}, browseSession(t, actor))
		if err != nil {
			return event.Event{}, err
		}
		if len(rows) != 1 || event.Tag(rows[0], "h") != room {
			break
		}
		root := rows[0]
		if root.Kind != 9 && root.Kind != 11 && root.Kind != 12 && root.Kind != 40002 {
			break
		}
		parent := event.RoomReplyRoot(root)
		if parent == "" {
			return root, nil
		}
		id = parent
	}
	return event.Event{}, errors.New("not found: thread")
}

// Follow indexed references so parent-only Buzz replies join the same thread
// as marked root/reply pairs. The traversal is bounded against hostile trees.
func (t *Tenant) roomThreadReplies(ctx context.Context, actor, room, root string) ([]event.Event, error) {
	known := map[string]bool{root: true}
	frontier := []string{root}
	var replies []event.Event
	for depth := 0; len(frontier) > 0 && depth < 64; depth++ {
		var nextLevel []string
		for start := 0; start < len(frontier); start += 100 {
			end := min(start+100, len(frontier))
			filter := event.Filter{Kinds: []int{9, 12}, Tags: map[string][]string{"h": {room}, "e": frontier[start:end]}}
			var cursor *storage.EventCursor
			for {
				rows, next, err := t.roomMessages(ctx, actor, filter, cursor, 100)
				if err != nil {
					return nil, err
				}
				for _, row := range rows {
					if known[row.ID] || !known[event.RoomReplyRoot(row)] {
						continue
					}
					known[row.ID] = true
					replies = append(replies, row)
					nextLevel = append(nextLevel, row.ID)
					if len(replies) > 5000 {
						return nil, errors.New("restricted: thread exceeds the browser limit")
					}
				}
				if next == "" {
					break
				}
				cursor, err = parseCollaborationCursor(next)
				if err != nil {
					return nil, err
				}
			}
		}
		frontier = nextLevel
	}
	if len(frontier) > 0 {
		return nil, errors.New("restricted: thread exceeds the nesting limit")
	}
	sortEventsNewestFirst(replies)
	return replies, nil
}

func roomThreadPage(rows []event.Event, cursor *storage.EventCursor, limit int) ([]event.Event, string) {
	page := make([]event.Event, 0, limit)
	for _, row := range rows {
		if cursor != nil && (row.CreatedAt > cursor.CreatedAt || row.CreatedAt == cursor.CreatedAt && row.ID <= cursor.ID) {
			continue
		}
		if len(page) == limit {
			return page, collaborationCursor(collaborationItemFrom(page[len(page)-1]))
		}
		page = append(page, row)
	}
	return page, ""
}

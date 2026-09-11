package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type directMessagesRequest struct {
	Cursor string `json:"cursor"`
	Limit  int    `json:"limit"`
}

func directMessagesBrowseMethod(method string) bool { return method == "browsedirectmessages" }

func (t *Tenant) executeDirectMessages(ctx context.Context, actor string, params []json.RawMessage) (any, error) {
	if actor == "" || !hex64(actor) {
		return nil, errors.New("auth-required: signed actor")
	}
	request := directMessagesRequest{Limit: 50}
	if len(params) > 0 {
		if err := json.Unmarshal(params[0], &request); err != nil {
			return nil, fmt.Errorf("invalid: direct message parameters: %w", err)
		}
	}
	if request.Limit <= 0 || request.Limit > 100 {
		request.Limit = 50
	}
	cursor, err := parseCollaborationCursor(request.Cursor)
	if err != nil {
		return nil, errors.New("invalid: direct message cursor")
	}
	return t.browseDirectMessages(ctx, actor, cursor, request.Limit)
}

func (t *Tenant) browseDirectMessages(ctx context.Context, actor string, cursor *storage.EventCursor, limit int) (any, error) {
	filter := event.Filter{Kinds: []int{event.KIND_WRAP}, Tags: map[string][]string{"p": {actor}}}
	session := relay.Session{PubKeys: []string{actor}, RelayURL: t.RelayURL()}
	if _, err := t.gate.Read(ctx, []event.Filter{filter}, session); err != nil {
		return nil, err
	}
	rows := make([]event.Event, 0, limit)
	next := ""
	for scans := 0; scans < 100; scans++ {
		page, err := t.store.Query(ctx, filter, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{PubKeys: []string{actor}}, Limit: 100, Before: cursor})
		if err != nil {
			return nil, err
		}
		for index, row := range page.Events {
			cursor = &storage.EventCursor{CreatedAt: row.CreatedAt, ID: row.ID}
			if !t.gate.CanSee(ctx, row, session, &filter) {
				continue
			}
			rows = append(rows, row)
			if len(rows) == limit {
				if page.More || index+1 < len(page.Events) {
					next = collaborationCursor(collaborationItem{CreatedAt: row.CreatedAt, ID: row.ID})
				}
				return map[string]any{"events": rows, "next_cursor": next}, nil
			}
		}
		if !page.More || len(page.Events) == 0 {
			return map[string]any{"events": rows, "next_cursor": ""}, nil
		}
	}
	if cursor != nil {
		next = collaborationCursor(collaborationItem{CreatedAt: cursor.CreatedAt, ID: cursor.ID})
	}
	return map[string]any{"events": rows, "next_cursor": next}, nil
}

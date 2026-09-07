package storage

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

type HistoryRow struct {
	Kind      int    `json:"kind"`
	D         string `json:"d"`
	EventID   string `json:"event_id"`
	CreatedAt int64  `json:"created_at"`
	SavedAt   int64  `json:"saved_at"`
}

func (s *Store) ListHistory(ctx context.Context, owner string, now int64) ([]HistoryRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT kind,d,event_id,created_at,saved_at FROM list_history WHERE owner=? AND(expires=0 OR expires>?) ORDER BY saved_at DESC,event_id", owner, now)
	if err != nil {
		return nil, fmt.Errorf("query list history: %w", err)
	}
	defer rows.Close()
	result := []HistoryRow{}
	for rows.Next() {
		var row HistoryRow
		if err := rows.Scan(&row.Kind, &row.D, &row.EventID, &row.CreatedAt, &row.SavedAt); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

type RestoreResult struct {
	Draft Draft `json:"draft"`
	Diff  Diff  `json:"diff"`
}
type Draft struct {
	Kind      int        `json:"kind"`
	CreatedAt int64      `json:"created_at"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
}
type Diff struct {
	AddedTags      [][]string `json:"addedTags"`
	RemovedTags    [][]string `json:"removedTags"`
	ContentChanged bool       `json:"contentChanged"`
}
type RestoreOptions struct {
	Owner   string
	EventID string
	Now     int64
}

func (s *Store) RestoreHistory(ctx context.Context, opts RestoreOptions) (RestoreResult, error) {
	var raw string
	var result RestoreResult
	err := s.db.QueryRowContext(ctx, "SELECT raw FROM list_history WHERE owner=? AND event_id=? AND(expires=0 OR expires>?)", opts.Owner, opts.EventID, opts.Now).Scan(&raw)
	if err != nil {
		return result, fmt.Errorf("load owned list history: %w", err)
	}
	var old event.Event
	if err := json.Unmarshal([]byte(raw), &old); err != nil {
		return result, err
	}
	f := event.Filter{Authors: []string{opts.Owner}, Kinds: []int{old.Kind}}
	if event.IsAddressable(old.Kind) {
		f.Tags = map[string][]string{"d": {event.Tag(old, "d")}}
	}
	current, err := s.Query(ctx, f, QueryOptions{Now: opts.Now, Access: Access{PubKeys: []string{opts.Owner}}, Limit: 1})
	if err != nil {
		return result, err
	}
	var cur event.Event
	if len(current.Events) > 0 {
		cur = current.Events[0]
	}
	at := opts.Now
	if cur.CreatedAt >= at {
		at = cur.CreatedAt + 1
	}
	result.Draft = Draft{Kind: old.Kind, CreatedAt: at, Tags: old.Tags, Content: old.Content}
	result.Diff.ContentChanged = cur.Content != old.Content
	result.Diff.AddedTags = tagDifference(old.Tags, cur.Tags)
	result.Diff.RemovedTags = tagDifference(cur.Tags, old.Tags)
	return result, nil
}

func tagDifference(first, second [][]string) [][]string {
	result := [][]string{}
	for _, a := range first {
		found := false
		for _, b := range second {
			if equalTag(a, b) {
				found = true
				break
			}
		}
		if !found {
			result = append(result, a)
		}
	}
	return result
}

func equalTag(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

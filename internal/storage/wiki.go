package storage

// Wiki revisions. A kind 30818 article is addressable, so a newer version
// from the same author replaces the older one in events. The replaced
// event is kept here with the id of the event that replaced it, until the
// author deletes it with a kind 5 naming the revision or the page.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// WikiRevision is one archived version of a wiki page.
type WikiRevision struct {
	ID           string
	Author       string
	D            string
	CreatedAt    int64
	SupersededBy string
	Event        event.Event
}

// WikiRevisions lists archived revisions newest first. An empty author or
// d matches every author or page.
func (s *Store) WikiRevisions(ctx context.Context, author, d string) ([]WikiRevision, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT event_id,author,d,created_at,superseded_by,raw FROM wiki_revisions WHERE (?='' OR author=?) AND (?='' OR d=?) ORDER BY created_at DESC,event_id", author, author, d, d)
	if err != nil {
		return nil, fmt.Errorf("query wiki revisions: %w", err)
	}
	defer rows.Close()
	result := []WikiRevision{}
	for rows.Next() {
		var row WikiRevision
		var raw string
		if err := rows.Scan(&row.ID, &row.Author, &row.D, &row.CreatedAt, &row.SupersededBy, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &row.Event); err != nil {
			return nil, fmt.Errorf("decode wiki revision %s: %w", row.ID, err)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// WikiRevision reads one archived revision by event id.
func (s *Store) WikiRevision(ctx context.Context, id string) (WikiRevision, bool, error) {
	var row WikiRevision
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT event_id,author,d,created_at,superseded_by,raw FROM wiki_revisions WHERE event_id=?", id).Scan(&row.ID, &row.Author, &row.D, &row.CreatedAt, &row.SupersededBy, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return row, false, nil
	}
	if err != nil {
		return row, false, fmt.Errorf("read wiki revision: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &row.Event); err != nil {
		return row, false, fmt.Errorf("decode wiki revision %s: %w", id, err)
	}
	return row, true, nil
}

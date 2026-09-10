package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// Access controls which private and callback events a query may return. All
// bypasses recipient filtering but still excludes hidden, pending and expired
// rows.
type Access struct {
	PubKeys []string
	All     bool
}

// QueryOptions supplies query time, access principals, an optional result
// limit and a cursor from a prior Query result.
type QueryOptions struct {
	Now    int64
	Access Access
	Limit  int
	Before *EventCursor
}

// EventCursor follows the stable newest-first query order.
type EventCursor struct {
	CreatedAt int64
	ID        string
}

// QueryResult contains the matching events and whether the limit hid more
// rows.
type QueryResult struct {
	Events []event.Event
	More   bool
}

type predicate struct {
	conditions []string
	args       []any
}

func (p *predicate) add(condition string, args ...any) {
	p.conditions = append(p.conditions, condition)
	p.args = append(p.args, args...)
}

func (p *predicate) list(column string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode filter list: %w", err)
	}
	p.add(column+" IN (SELECT value FROM json_each(?))", string(b))
	return nil
}

func where(f event.Filter, opts QueryOptions) (predicate, error) {
	var p predicate
	if opts.Before != nil {
		p.add("(created_at<? OR (created_at=? AND id>?))", opts.Before.CreatedAt, opts.Before.CreatedAt, opts.Before.ID)
	}
	if f.IDs != nil {
		if err := p.list("id", f.IDs); err != nil {
			return p, err
		}
	}
	if f.Authors != nil {
		if err := p.list("pubkey", f.Authors); err != nil {
			return p, err
		}
	}
	if f.Kinds != nil {
		if err := p.list("kind", f.Kinds); err != nil {
			return p, err
		}
	}
	if f.Since != nil {
		p.add("created_at>=?", *f.Since)
	}
	if f.Until != nil {
		p.add("created_at<=?", *f.Until)
	}
	keys := make([]string, 0, len(f.Tags))
	for name := range f.Tags {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		values, err := json.Marshal(f.Tags[name])
		if err != nil {
			return p, fmt.Errorf("encode tag filter: %w", err)
		}
		p.add("EXISTS(SELECT 1 FROM tags WHERE tags.event_id=events.id AND name=? AND value IN(SELECT value FROM json_each(?)))", name, string(values))
	}
	terms := event.SearchTerms(f.Search)
	if len(terms) > 0 {
		quoted := make([]string, len(terms))
		for i, term := range terms {
			quoted[i] = `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
		}
		p.add("seq IN (SELECT rowid FROM search WHERE search MATCH ?)", strings.Join(quoted, " AND "))
	}
	p.add("(expires=0 OR expires>?)", opts.Now)
	p.add("NOT EXISTS(SELECT 1 FROM hidden_events WHERE hidden_events.id=events.id)")
	p.add("NOT EXISTS(SELECT 1 FROM pending_events WHERE pending_events.id=events.id)")
	if err := addAccess(&p, opts.Access); err != nil {
		return p, err
	}
	return p, nil
}

func addAccess(p *predicate, who Access) error {
	keys, err := json.Marshal(who.PubKeys)
	if err != nil {
		return fmt.Errorf("encode read principals: %w", err)
	}
	// Callback registrations never become visible through internal all-access.
	p.add("(kind<>"+fmt.Sprint(event.KIND_PUSH_REGISTRATION)+" OR pubkey IN(SELECT value FROM json_each(?)))", string(keys))
	if who.All {
		return nil
	}
	// NIP-59 seals (kind 13) carry no recipient tag; only their author may
	// retrieve them. Gift wraps and other private events are recipient scoped.
	p.add("(kind NOT IN(4,13,1059,21059,24133) OR pubkey IN(SELECT value FROM json_each(?)) OR (kind<>13 AND EXISTS(SELECT 1 FROM tags WHERE event_id=events.id AND name='p' AND value IN(SELECT value FROM json_each(?)))))", string(keys), string(keys))
	return nil
}

func (s *Store) Query(ctx context.Context, f event.Filter, opts QueryOptions) (result QueryResult, err error) {
	started := time.Now()
	defer func() { s.observe("query", started, err) }()
	p, err := where(f, opts)
	if err != nil {
		return result, err
	}
	query := "SELECT raw FROM events WHERE " + strings.Join(p.conditions, " AND ") + " ORDER BY created_at DESC,id ASC"
	limit := opts.Limit
	if f.Limit != nil && (limit <= 0 || *f.Limit < limit) {
		limit = *f.Limit
	}
	limited := opts.Limit > 0 || f.Limit != nil
	if limited {
		if limit < 0 {
			limit = 0
		}
		query += " LIMIT ?"
		p.args = append(p.args, int64(limit)+1)
	}
	rows, err := s.db.QueryContext(ctx, query, p.args...)
	if err != nil {
		return result, fmt.Errorf("query events: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
	}()
	result.Events = []event.Event{}
	for rows.Next() {
		if limited && len(result.Events) == limit {
			result.More = true
			break
		}
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return result, fmt.Errorf("read event: %w", err)
		}
		var e event.Event
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return result, fmt.Errorf("decode stored event: %w", err)
		}
		result.Events = append(result.Events, e)
	}
	return result, rows.Err()
}

func (s *Store) Count(ctx context.Context, filters []event.Filter, opts QueryOptions) (count int64, err error) {
	started := time.Now()
	defer func() { s.observe("count", started, err) }()
	if len(filters) == 0 {
		return 0, nil
	}
	var ors []string
	var args []any
	for _, f := range filters {
		p, err := where(f, opts)
		if err != nil {
			return 0, err
		}
		ors = append(ors, "("+strings.Join(p.conditions, " AND ")+")")
		args = append(args, p.args...)
	}
	err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE "+strings.Join(ors, " OR "), args...).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count events: %w", err)
	}
	return count, nil
}

type CursorEvent struct {
	Sequence int64       `json:"seq"`
	Event    event.Event `json:"event"`
}

func (s *Store) After(ctx context.Context, seq int64, f event.Filter, opts QueryOptions) ([]CursorEvent, error) {
	p, err := where(f, opts)
	if err != nil {
		return nil, err
	}
	p.add("seq>?", seq)
	q := "SELECT seq,raw FROM events WHERE " + strings.Join(p.conditions, " AND ") + " ORDER BY seq"
	if opts.Limit > 0 {
		q += " LIMIT ?"
		p.args = append(p.args, opts.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, p.args...)
	if err != nil {
		return nil, fmt.Errorf("query event cursor: %w", err)
	}
	defer rows.Close()
	var result []CursorEvent
	for rows.Next() {
		var item CursorEvent
		var raw string
		if err := rows.Scan(&item.Sequence, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &item.Event); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

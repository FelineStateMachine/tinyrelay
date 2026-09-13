package gitstore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"

	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
)

// ErrDuplicate indicates that an event was already persisted.
var ErrDuplicate = errors.New("duplicate event")

// Store adapts the shared SQLite event store to the Git engine contract.
type Store struct{ store *storage.Store }

// Open creates a caller-owned SQLite store.
func Open(ctx context.Context, path string) (*Store, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	store, err := storage.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	return &Store{store: store}, nil
}

// Wrap adapts an existing host store without taking lifecycle ownership.
func Wrap(store *storage.Store) *Store { return &Store{store: store} }

func (s *Store) Query(ctx context.Context, f nostr.Filter, now int64, limit int) ([]nostr.Event, error) {
	result, err := s.store.Query(ctx, f, storage.QueryOptions{Now: now, Limit: limit, Access: storage.Access{All: true}})
	return result.Events, err
}
func (s *Store) Save(ctx context.Context, e nostr.Event, now int64) error {
	_, err := s.store.Save(ctx, e, storage.SaveOptions{Now: now})
	if errors.Is(err, storage.ErrDuplicate) {
		return ErrDuplicate
	}
	return err
}
func (s *Store) Latest(ctx context.Context, kind int, pubkey, identifier string) (nostr.Event, bool, error) {
	var raw string
	err := s.store.DB().QueryRowContext(ctx, "SELECT raw FROM events WHERE kind=? AND pubkey=? AND d=? ORDER BY created_at DESC,id ASC LIMIT 1", kind, pubkey, identifier).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nostr.Event{}, false, nil
	}
	if err != nil {
		return nostr.Event{}, false, err
	}
	event, err := nostr.Parse([]byte(raw))
	if err != nil {
		return nostr.Event{}, false, err
	}
	return event, true, nil
}
func (s *Store) Exists(ctx context.Context, id string, kind int) (bool, error) {
	var found int
	err := s.store.DB().QueryRowContext(ctx, "SELECT 1 FROM events WHERE id=? AND kind=?", id, kind).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
func (s *Store) Close() error { return s.store.Close() }

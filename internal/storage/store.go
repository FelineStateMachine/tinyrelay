// Package storage owns each tenant's local SQLite database and transaction boundary.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Rejection string

func (e Rejection) Error() string { return string(e) }

const (
	ErrDuplicate Rejection = "duplicate: already have this event"
	ErrReplaced  Rejection = "duplicate: a newer version exists"
	ErrDeleted   Rejection = "blocked: event was previously deleted by its author"
	ErrVanished  Rejection = "blocked: this pubkey asked for its events to be deleted"
)

type Observer interface {
	ObserveStorage(string, string, time.Duration)
}

type Store struct {
	db         *sql.DB
	path       string
	observerMu sync.RWMutex
	observer   Observer
}

func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("create database: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close database file: %w", err)
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	for _, pragma := range []string{"journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(ON)", "busy_timeout(5000)"} {
		q.Add("_pragma", pragma)
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// One connection is the initial serialization boundary. Reader pools can be
	// introduced after measurements without changing transaction semantics.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, path: path}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, errors.Join(fmt.Errorf("initialize tenant schema: %w", err), db.Close())
	}
	return s, nil
}

func (s *Store) DB() *sql.DB  { return s.db }
func (s *Store) Path() string { return s.path }
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) SetObserver(o Observer) {
	s.observerMu.Lock()
	defer s.observerMu.Unlock()
	s.observer = o
}

func (s *Store) observe(operation string, started time.Time, err error) {
	s.observerMu.RLock()
	defer s.observerMu.RUnlock()
	if s.observer == nil {
		return
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	s.observer.ObserveStorage(operation, outcome, time.Since(started))
}

// WithTx must not call another Store method: the callback owns the connection.
// A successful return guarantees all domain changes and work intents committed.
func (s *Store) WithTx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	started := time.Now()
	defer func() { s.observe("commit", started, err) }()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tenant transaction: %w", err)
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tenant transaction: %w", err)
	}
	return nil
}

func exists(ctx context.Context, tx *sql.Tx, query string, args ...any) (bool, error) {
	var value int
	err := tx.QueryRowContext(ctx, query, args...).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

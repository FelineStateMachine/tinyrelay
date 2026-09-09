package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

type Stats struct {
	Events   int64 `json:"events"`
	Bytes    int64 `json:"bytes"`
	Oldest   int64 `json:"oldest"`
	Newest   int64 `json:"newest"`
	WALBytes int64 `json:"wal_bytes"`
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var result Stats
	err := s.db.QueryRowContext(ctx, "SELECT count(*),coalesce(min(created_at),0),coalesce(max(created_at),0) FROM events WHERE kind<>?", event.KIND_PUSH_REGISTRATION).Scan(&result.Events, &result.Oldest, &result.Newest)
	if err != nil {
		return result, fmt.Errorf("read storage stats: %w", err)
	}
	var pages, size int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return result, err
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size); err != nil {
		return result, err
	}
	result.Bytes = pages * size
	if info, err := os.Stat(s.path + "-wal"); err == nil {
		result.WALBytes = info.Size()
	} else if !os.IsNotExist(err) {
		return result, err
	}
	return result, nil
}

func (s *Store) Vanish(ctx context.Context, pubkey string, until int64) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		statements := []struct {
			sql  string
			args []any
		}{
			{"INSERT INTO vanished(pubkey,until) VALUES(?,?) ON CONFLICT(pubkey) DO UPDATE SET until=max(until,excluded.until)", []any{pubkey, until}},
			{"DELETE FROM events WHERE pubkey=? AND created_at<=?", []any{pubkey, until}},
			{"DELETE FROM events WHERE kind=1059 AND id IN(SELECT event_id FROM tags WHERE name='p' AND value=?)", []any{pubkey}},
			{"DELETE FROM list_history WHERE owner=?", []any{pubkey}},
			{"DELETE FROM wiki_revisions WHERE author=?", []any{pubkey}},
		}
		for _, q := range statements {
			if _, err := tx.ExecContext(ctx, q.sql, q.args...); err != nil {
				return fmt.Errorf("vanish events: %w", err)
			}
		}
		return nil
	})
}

func (s *Store) SweepExpired(ctx context.Context, now int64) (int64, error) {
	var next int64
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		for _, table := range []string{"events", "list_history"} {
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE expires>0 AND expires<=?", now); err != nil {
				return fmt.Errorf("sweep expiration: %w", err)
			}
		}
		return tx.QueryRowContext(ctx, "SELECT coalesce(min(expires),0) FROM events WHERE expires>0").Scan(&next)
	})
	return next, err
}

func (s *Store) DeleteEvent(ctx context.Context, id string) (bool, error) {
	result, err := s.db.ExecContext(ctx, "DELETE FROM events WHERE id=?", id)
	if err != nil {
		return false, fmt.Errorf("delete stored event: %w", err)
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

func (s *Store) EraseAuthor(ctx context.Context, pubkey string) (int64, error) {
	var n int64
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "DELETE FROM events WHERE pubkey=?", pubkey)
		if err != nil {
			return err
		}
		n, err = result.RowsAffected()
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM list_history WHERE owner=?", pubkey); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM wiki_revisions WHERE author=?", pubkey)
		return err
	})
	return n, err
}

func (s *Store) GetSetting(ctx context.Context, key string, dest any) error {
	var raw string
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", key).Scan(&raw); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(raw), dest); err != nil {
		return fmt.Errorf("decode setting %s: %w", key, err)
	}
	return nil
}

func (s *Store) PutSetting(ctx context.Context, key string, value any) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error { return PutSetting(ctx, tx, key, value) })
}

// DeleteSetting removes one settings row. A missing row is not an error.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM settings WHERE key=?", key)
	return err
}

// PruneSettings removes rows under prefix whose keys are not in keep, so
// per-filter state cannot accumulate as filters come and go.
func (s *Store) PruneSettings(ctx context.Context, prefix string, keep map[string]struct{}) error {
	rows, err := s.db.QueryContext(ctx, "SELECT key FROM settings WHERE substr(key,1,?)=?", len(prefix), prefix)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return err
		}
		if _, ok := keep[key]; !ok {
			stale = append(stale, key)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, key := range stale {
		if err := s.DeleteSetting(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func PutSetting(ctx context.Context, tx *sql.Tx, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode setting %s: %w", key, err)
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, string(raw))
	if err != nil {
		return fmt.Errorf("save setting %s: %w", key, err)
	}
	return nil
}

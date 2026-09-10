package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// Intent describes durable work to insert with an event save. The tuple of
// Kind, EventID and Target determines its stable ID; Payload is retained for
// the worker handling that kind.
type Intent struct {
	Kind    string `json:"kind"`
	EventID string `json:"event_id"`
	Target  string `json:"target"`
	Payload string `json:"payload"`
}

// SaveOptions controls event persistence. Now is the Unix time used for
// history and intent timestamps. SearchMode is empty for the default indexed
// kinds, "full" for all public content and "off" to skip content indexing.
// For stored events, Intents and BeforeCommit run in the event transaction.
// Ephemeral events return before those hooks. A BeforeCommit error rolls back
// the transaction owned by Save; SaveTx returns it to the transaction's owner.
type SaveOptions struct {
	Now          int64
	SearchMode   string
	Intents      []Intent
	BeforeCommit func(context.Context, *sql.Tx) error
}

// SaveResult reports the event sequence. Ephemeral events return without a
// database row and set Ephemeral.
type SaveResult struct {
	Sequence  int64
	Ephemeral bool
}

// Save commits an event, its indexes and the supplied transaction hooks
// together. Callers validate the event before saving. Duplicate, replaced,
// deleted and vanished events return a Rejection. Ephemeral events return an
// Ephemeral result without writing rows or running the hooks.
func (s *Store) Save(ctx context.Context, e event.Event, opts SaveOptions) (SaveResult, error) {
	var result SaveResult
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = SaveTx(ctx, tx, e, opts)
		return err
	})
	return result, err
}

// SaveTx composes event persistence with membership or other tenant changes.
// The caller owns commit/rollback; no success may be acknowledged before commit.
func SaveTx(ctx context.Context, tx *sql.Tx, e event.Event, opts SaveOptions) (SaveResult, error) {
	if event.IsEphemeral(e.Kind) {
		return SaveResult{Ephemeral: true}, nil
	}
	var result SaveResult
	err := func() error {
		if err := checkSave(ctx, tx, e); err != nil {
			return err
		}
		if err := replaceOrDelete(ctx, tx, e, opts.Now); err != nil {
			return err
		}
		seq, err := insertEvent(ctx, tx, e, opts.SearchMode)
		if err != nil {
			return err
		}
		result.Sequence = seq
		if err := AddIntents(ctx, tx, opts.Intents, opts.Now); err != nil {
			return err
		}
		if opts.BeforeCommit != nil {
			return opts.BeforeCommit(ctx, tx)
		}
		return nil
	}()
	return result, err
}

func checkSave(ctx context.Context, tx *sql.Tx, e event.Event) error {
	checks := []struct {
		query     string
		args      []any
		rejection Rejection
	}{
		{"SELECT 1 FROM events WHERE id=?", []any{e.ID}, ErrDuplicate},
		{"SELECT 1 FROM vanished WHERE pubkey=? AND until>=?", []any{e.PubKey, e.CreatedAt}, ErrVanished},
	}
	for _, check := range checks {
		has, err := exists(ctx, tx, check.query, check.args...)
		if err != nil {
			return fmt.Errorf("check event persistence: %w", err)
		}
		if has {
			return check.rejection
		}
	}
	return checkDeleted(ctx, tx, e)
}

func checkDeleted(ctx context.Context, tx *sql.Tx, e event.Event) error {
	deleters := []string{e.PubKey}
	if e.Kind == 1059 {
		deleters = append(deleters, event.TagValues(e, "p")...)
	}
	list, err := json.Marshal(deleters)
	if err != nil {
		return fmt.Errorf("encode deletion principals: %w", err)
	}
	has, err := exists(ctx, tx, "SELECT 1 FROM deletions WHERE target_type='e' AND target=? AND author IN (SELECT value FROM json_each(?)) LIMIT 1", e.ID, string(list))
	if err != nil {
		return fmt.Errorf("check deletion tombstone: %w", err)
	}
	if has {
		return ErrDeleted
	}
	if !event.IsAddressable(e.Kind) {
		return nil
	}
	address := strconv.Itoa(e.Kind) + ":" + e.PubKey + ":" + event.Tag(e, "d")
	has, err = exists(ctx, tx, "SELECT 1 FROM deletions WHERE author=? AND target_type='a' AND target=? AND until>=?", e.PubKey, address, e.CreatedAt)
	if err != nil {
		return fmt.Errorf("check address tombstone: %w", err)
	}
	if has {
		return ErrDeleted
	}
	return nil
}

func replaceOrDelete(ctx context.Context, tx *sql.Tx, e event.Event, now int64) error {
	if event.IsReplaceable(e.Kind) || event.IsAddressable(e.Kind) {
		return replace(ctx, tx, e, now)
	}
	if e.Kind != 5 {
		return nil
	}
	for _, tag := range e.Tags {
		if len(tag) < 2 || (tag[0] != "e" && tag[0] != "a") {
			continue
		}
		if err := applyDeletion(ctx, tx, e, tag); err != nil {
			return err
		}
	}
	return nil
}

func replace(ctx context.Context, tx *sql.Tx, e event.Event, now int64) error {
	d := ""
	if event.IsAddressable(e.Kind) {
		d = event.Tag(e, "d")
	}
	has, err := exists(ctx, tx, "SELECT 1 FROM events WHERE pubkey=? AND kind=? AND d=? AND (created_at>? OR (created_at=? AND id<?)) LIMIT 1", e.PubKey, e.Kind, d, e.CreatedAt, e.CreatedAt, e.ID)
	if err != nil {
		return fmt.Errorf("check replaceable event: %w", err)
	}
	if has {
		return ErrReplaced
	}
	if isListKind(e.Kind) {
		_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO list_history(owner,kind,d,event_id,created_at,saved_at,expires,raw) SELECT pubkey,kind,d,id,created_at,?,expires,raw FROM events WHERE pubkey=? AND kind=? AND d=?", now, e.PubKey, e.Kind, d)
		if err != nil {
			return fmt.Errorf("archive replaced list: %w", err)
		}
	}
	if e.Kind == kindWikiArticle {
		// A wiki page keeps its history: the version being replaced moves
		// to wiki_revisions with the id of the event that replaced it.
		_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO wiki_revisions(event_id,author,d,created_at,raw,superseded_by) SELECT id,pubkey,d,created_at,raw,? FROM events WHERE pubkey=? AND kind=? AND d=?", e.ID, e.PubKey, e.Kind, d)
		if err != nil {
			return fmt.Errorf("archive replaced wiki revision: %w", err)
		}
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM events WHERE pubkey=? AND kind=? AND d=?", e.PubKey, e.Kind, d)
	if err != nil {
		return fmt.Errorf("replace event: %w", err)
	}
	return nil
}

func applyDeletion(ctx context.Context, tx *sql.Tx, e event.Event, tag []string) error {
	if tag[0] == "a" {
		parts := strings.SplitN(tag[1], ":", 3)
		if len(parts) != 3 || parts[1] != e.PubKey {
			return nil
		}
		kind, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM events WHERE kind=? AND pubkey=? AND d=? AND created_at<=?", kind, e.PubKey, parts[2], e.CreatedAt); err != nil {
			return fmt.Errorf("delete address: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM list_history WHERE kind=? AND owner=? AND d=? AND created_at<=?", kind, e.PubKey, parts[2], e.CreatedAt); err != nil {
			return fmt.Errorf("delete list history: %w", err)
		}
		if kind == kindWikiArticle {
			if _, err := tx.ExecContext(ctx, "DELETE FROM wiki_revisions WHERE author=? AND d=? AND created_at<=?", e.PubKey, parts[2], e.CreatedAt); err != nil {
				return fmt.Errorf("delete wiki revisions: %w", err)
			}
		}
	} else {
		if _, err := tx.ExecContext(ctx, "DELETE FROM events WHERE id=? AND (pubkey=? OR (kind=1059 AND EXISTS(SELECT 1 FROM tags WHERE event_id=events.id AND name='p' AND value=?)))", tag[1], e.PubKey, e.PubKey); err != nil {
			return fmt.Errorf("delete event: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM list_history WHERE owner=? AND event_id=?", e.PubKey, tag[1]); err != nil {
			return fmt.Errorf("delete list version: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM wiki_revisions WHERE author=? AND event_id=?", e.PubKey, tag[1]); err != nil {
			return fmt.Errorf("delete wiki revision: %w", err)
		}
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO deletions(author,target_type,target,until) VALUES(?,?,?,?) ON CONFLICT(author,target_type,target) DO UPDATE SET until=max(until,excluded.until)", e.PubKey, tag[0], tag[1], e.CreatedAt)
	if err != nil {
		return fmt.Errorf("persist deletion tombstone: %w", err)
	}
	return nil
}

func insertEvent(ctx context.Context, tx *sql.Tx, e event.Event, mode string) (int64, error) {
	raw, err := event.Canonical(e)
	if err != nil {
		return 0, fmt.Errorf("encode event: %w", err)
	}
	d := ""
	if event.IsAddressable(e.Kind) {
		d = event.Tag(e, "d")
	}
	var seq int64
	err = tx.QueryRowContext(ctx, "INSERT INTO events(id,pubkey,created_at,kind,d,expires,raw) VALUES(?,?,?,?,?,?,?) RETURNING seq", e.ID, e.PubKey, e.CreatedAt, e.Kind, d, event.Expiration(e), string(raw)).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("insert event: %w", err)
	}
	for _, tag := range e.Tags {
		if len(tag) < 2 || len(tag[0]) != 1 {
			continue
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO tags(event_id,name,value) VALUES(?,?,?)", e.ID, tag[0], tag[1]); err != nil {
			return 0, fmt.Errorf("index event tag: %w", err)
		}
	}
	if e.Content != "" && indexed(e.Kind, mode) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO search(rowid,content) VALUES(?,?)", seq, e.Content); err != nil {
			return 0, fmt.Errorf("index event content: %w", err)
		}
	}
	return seq, nil
}

func indexed(kind int, mode string) bool {
	if event.IsPrivate(kind) || mode == "off" {
		return false
	}
	if mode == "full" {
		return true
	}
	switch kind {
	case 0, 1, 11, 1111, 9802, 30023, 30024, 30818:
		return true
	}
	return false
}

func isListKind(kind int) bool { return kind == 3 || kind == 10002 || kind == 10003 || kind == 30003 }

// kindWikiArticle is the NIP-54 article kind, whose replaced versions are
// kept as revisions.
const kindWikiArticle = 30818

func AddIntents(ctx context.Context, tx *sql.Tx, intents []Intent, now int64) error {
	for _, intent := range intents {
		_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO work_intents(id,kind,event_id,target,payload,next_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)", IntentID(intent.Kind, intent.EventID, intent.Target), intent.Kind, intent.EventID, intent.Target, intent.Payload, now, now, now)
		if err != nil {
			return fmt.Errorf("enqueue %s intent: %w", intent.Kind, err)
		}
	}
	return nil
}

// IntentID returns the stable identifier shared by transactional and queued
// durable work. Payload changes do not create a second intent.
func IntentID(kind, eventID, target string) string {
	key := sha256.Sum256([]byte(kind + "\x00" + eventID + "\x00" + target))
	return hex.EncodeToString(key[:])
}

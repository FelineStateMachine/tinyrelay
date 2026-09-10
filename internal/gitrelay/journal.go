package gitrelay

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type journalRecord struct {
	Repository string            `json:"repository"`
	EventID    string            `json:"event_id"`
	Kind       int               `json:"kind"`
	Refs       map[string]string `json:"refs,omitempty"`
	Head       string            `json:"head,omitempty"`
	Committed  bool              `json:"committed"`
}

func (g *GitRelay) journalDir() string { return filepath.Join(g.root, ".tinyrelay", "journal") }

func (g *GitRelay) journalPath(r journalRecord) string {
	h := sha256.Sum256([]byte(r.Repository + "\x00" + r.EventID))
	return filepath.Join(g.journalDir(), hex.EncodeToString(h[:])+".json")
}

func (g *GitRelay) writeJournal(repo Repository, r journalRecord) error {
	if err := os.MkdirAll(g.journalDir(), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := g.journalPath(r) + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, g.journalPath(r))
}

func (g *GitRelay) commitJournal(repo Repository, r journalRecord) error {
	if err := os.Remove(g.journalPath(r)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// recoverJournals completes state publication after a process crash. A
// journal is deliberately small and repository-local; the event store remains
// authoritative for event durability, while the file is the Git authority.
func (g *GitRelay) recoverJournals() error {
	entries, err := os.ReadDir(g.journalDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(g.journalDir(), entry.Name()))
		if err != nil {
			return err
		}
		var jr journalRecord
		if err := json.Unmarshal(b, &jr); err != nil {
			return fmt.Errorf("git relay journal %s: %w", entry.Name(), err)
		}
		owner, id, ok := strings.Cut(jr.Repository, "\x00")
		if !ok || owner == "" || id == "" {
			return fmt.Errorf("git relay journal %s: invalid repository", entry.Name())
		}
		r := Repository{Owner: owner, Identifier: id, Refs: jr.Refs, Head: jr.Head}
		if err := g.ensureRepo(r); err != nil {
			return err
		}
		keepJournal := false
		if jr.Kind == 30618 {
			var current string
			err := g.store.DB().QueryRowContext(context.Background(), `SELECT events.id FROM events JOIN tags ON tags.event_id=events.id AND tags.name='d' AND tags.value=? WHERE events.pubkey=? AND events.kind=30618 ORDER BY events.created_at DESC,events.id ASC LIMIT 1`, id, owner).Scan(&current)
			if errors.Is(err, sql.ErrNoRows) || current != jr.EventID {
				if removeErr := os.Remove(filepath.Join(g.journalDir(), entry.Name())); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					return removeErr
				}
				continue
			}
			if err != nil {
				return err
			}
			if g.stateObjectsPresent(context.Background(), r) {
				err = g.writeState(r)
			} else {
				err = g.writePendingState(r)
				keepJournal = true
			}
			if err != nil {
				return err
			}
			g.mu.Lock()
			g.pending[jr.EventID] = struct{}{}
			g.mu.Unlock()
		} else if jr.Kind == 1617 || jr.Kind == 1618 || jr.Kind == 1619 {
			if !g.stateObjectsPresent(context.Background(), r) {
				if err := g.writePendingState(r); err != nil {
					return err
				}
				keepJournal = true
				g.mu.Lock()
				g.pending[jr.EventID] = struct{}{}
				g.mu.Unlock()
			}
		}
		if !keepJournal {
			if err := os.Remove(filepath.Join(g.journalDir(), entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

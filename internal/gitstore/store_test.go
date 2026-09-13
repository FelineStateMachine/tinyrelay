package gitstore

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
)

func TestStorePreservesDurableMetadataAndReadVisibility(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	e := nostr.Event{Kind: 30618, CreatedAt: 1, Tags: [][]string{{"d", "repo"}}}
	if err := nostr.Sign(&e, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, e, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, e, 1); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate save: %v", err)
	}
	if found, err := store.Exists(ctx, e.ID, 30618); err != nil || !found {
		t.Fatalf("exists=%v, error=%v", found, err)
	}
	if _, err := store.store.DB().ExecContext(ctx, `INSERT INTO pending_events(id,reason) VALUES(?,'git')`, e.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := store.Query(ctx, nostr.Filter{Kinds: []int{30618}}, 1, 0)
	if err != nil || len(rows) != 0 {
		t.Fatalf("pending query=%v, error=%v", rows, err)
	}
	latest, found, err := store.Latest(ctx, 30618, e.PubKey, "repo")
	if err != nil || !found || latest.ID != e.ID {
		t.Fatalf("latest=%v, found=%v, error=%v", latest, found, err)
	}
	if _, err := store.store.DB().ExecContext(ctx, `UPDATE events SET raw='{}' WHERE id=?`, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Latest(ctx, 30618, e.PubKey, "repo"); err == nil {
		t.Fatal("accepted corrupt stored metadata")
	}
}

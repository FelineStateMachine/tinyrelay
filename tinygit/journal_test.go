package tinygit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
)

type unavailableMetadataStore struct {
	Store
	err error
}

func (s unavailableMetadataStore) Latest(context.Context, int, string, string) (Event, bool, error) {
	return Event{}, false, s.err
}

func TestJournalRecoveryPreservesIntentWhenStorageReadFails(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := OpenStore(ctx, filepath.Join(root, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git")})
	if err != nil {
		t.Fatal(err)
	}
	state := Event{Kind: 30618, CreatedAt: 1, Tags: [][]string{{"d", "demo"}, {"refs/heads/main", strings.Repeat("c", 40)}}}
	if err := nostr.Sign(&state, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, state, 1); err != nil {
		t.Fatal(err)
	}
	repo := Repository{Owner: state.PubKey, Identifier: "demo", Refs: map[string]string{"refs/heads/main": strings.Repeat("c", 40)}}
	journal := journalRecord{Repository: key(repo.Owner, repo.Identifier), EventID: state.ID, Kind: 30618, Refs: repo.Refs}
	if err := g.writeJournal(repo, journal); err != nil {
		t.Fatal(err)
	}
	unavailable := errors.New("metadata temporarily unavailable")
	g.store = unavailableMetadataStore{Store: store, err: unavailable}
	if err := g.recoverJournals(); !errors.Is(err, unavailable) {
		t.Errorf("recovery error=%v; want storage failure", err)
	}
	if _, err := os.Stat(g.journalPath(journal)); err != nil {
		t.Fatalf("recovery discarded the journal after a read failure: %v", err)
	}
	g.store = store
	if err := g.recoverJournals(); err != nil {
		t.Fatal(err)
	}
	pending, err := os.ReadFile(filepath.Join(g.repoPath(repo), "tinyrelay.pending"))
	if err != nil || !strings.Contains(string(pending), "refs/heads/main "+strings.Repeat("c", 40)) {
		t.Fatalf("retry did not recover pending state: %q, %v", pending, err)
	}
}

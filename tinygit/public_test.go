package tinygit_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
	"github.com/FelineStateMachine/tinyrelay/tinygit"
)

func TestPublicStoreAcceptsRelativePath(t *testing.T) {
	t.Chdir(t.TempDir())
	store, err := tinygit.OpenStore(context.Background(), "events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Query(context.Background(), nostr.Filter{}, 0, 0); err != nil {
		t.Fatal(err)
	}
}

func TestPublicEngineCanOpenWithoutInternalImports(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := tinygit.OpenStore(ctx, filepath.Join(root, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p := tinygit.DefaultPolicy("")
	g, err := tinygit.New(tinygit.Config{
		Store: store, Root: filepath.Join(root, "git"),
		Policy:    func() tinygit.Policy { return p },
		Authorize: func(context.Context, tinygit.Event, tinygit.Repository) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.ValidateEvent(tinygit.Event{}); err == nil {
		t.Fatal("unsigned metadata accepted")
	}
}

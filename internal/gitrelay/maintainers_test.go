package gitrelay

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type maintainerFunc func(context.Context, Repository, string) bool

func (f maintainerFunc) IsMaintainer(ctx context.Context, r Repository, pubkey string) bool {
	return f(ctx, r, pubkey)
}

// The host's maintainer answer is consulted after the owner and the
// maintainers tag, and it decides whether a state event from another key is
// accepted.
func TestMaintainerSourceDecidesStateAuthority(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	vouched := map[string]bool{}
	var asked []string
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git"), Maintainers: maintainerFunc(func(_ context.Context, r Repository, pubkey string) bool {
		asked = append(asked, r.Identifier+":"+pubkey)
		return vouched[pubkey]
	})})
	if err != nil {
		t.Fatal(err)
	}
	ownerSecret, agentSecret := strings.Repeat("0", 63)+"1", strings.Repeat("0", 63)+"2"
	owner, _ := event.PublicKey(ownerSecret)
	agent, _ := event.PublicKey(agentSecret)
	announcement := event.Event{Kind: 30617, CreatedAt: 100, Tags: [][]string{{"d", "demo"}, {"clone", "https://relay.example/" + owner + "/demo.git"}}}
	if err := event.Sign(&announcement, ownerSecret); err != nil {
		t.Fatal(err)
	}
	if err := g.Publish(context.Background(), announcement); err != nil {
		t.Fatal(err)
	}
	state := func(createdAt int64) event.Event {
		e := event.Event{Kind: 30618, CreatedAt: createdAt, Tags: [][]string{{"d", "demo"}, {"HEAD", "ref: refs/heads/main"}, {"refs/heads/main", strings.Repeat("a", 40)}}}
		if err := event.Sign(&e, agentSecret); err != nil {
			t.Fatal(err)
		}
		return e
	}
	if _, err := g.Validate(context.Background(), state(101)); err == nil || !strings.Contains(err.Error(), "not an accepted repository maintainer") {
		t.Fatalf("state from an unvouched key: %v", err)
	}
	if len(asked) == 0 || asked[0] != "demo:"+agent {
		t.Fatalf("source was not consulted for the repository: %v", asked)
	}
	vouched[agent] = true
	repo, err := g.Validate(context.Background(), state(102))
	if err != nil {
		t.Fatalf("state from a vouched key: %v", err)
	}
	if repo.Owner != owner || repo.Refs["refs/heads/main"] != strings.Repeat("a", 40) {
		t.Fatalf("state bound to the wrong repository: %+v", repo)
	}
	if !g.IsMaintainer(context.Background(), repo, owner) || g.IsMaintainer(context.Background(), repo, "") {
		t.Fatal("owner must count and an empty key must not")
	}
	vouched[agent] = false
	if _, err := g.Validate(context.Background(), state(103)); err == nil {
		t.Fatal("a withdrawn answer must reject the next state")
	}
}

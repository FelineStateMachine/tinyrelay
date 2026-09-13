package tinygit

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestValidMetadataShapeDoesNotGrantAdmission(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	calls := 0
	denied := errors.New("blocked: host does not accept this repository")
	g, err := New(Config{Store: store, Root: filepath.Join(t.TempDir(), "git"), Authorize: func(context.Context, Event, Repository) error {
		calls++
		return denied
	}})
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("0", 63) + "1"
	for _, kind := range []int{30617, 30618} {
		e := Event{Kind: kind, CreatedAt: 1, Tags: [][]string{{"d", "demo"}}}
		if err := event.Sign(&e, secret); err != nil {
			t.Fatal(err)
		}
		if _, err := ParseMetadata(e); err != nil {
			t.Fatalf("valid shape rejected: %v", err)
		}
		if kind == 30618 {
			// Pure parsing needs no announcement; admission still does.
			if _, err := g.Validate(ctx, e); err == nil || !strings.Contains(err.Error(), "announcement is missing") {
				t.Fatalf("state without authority admitted: %v", err)
			}
			g.repos[key(e.PubKey, "demo")] = Repository{Owner: e.PubKey, Identifier: "demo", EventID: "announcement"}
		}
		for _, admit := range []func(context.Context, Event) error{
			func(ctx context.Context, e Event) error { _, err := g.Validate(ctx, e); return err },
			func(ctx context.Context, e Event) error { _, err := g.ValidateImported(ctx, e); return err },
			g.Publish,
		} {
			calls = 0
			if err := admit(ctx, e); !errors.Is(err, denied) || calls != 1 {
				t.Fatalf("shape bypassed host admission: kind=%d calls=%d err=%v", kind, calls, err)
			}
			// A tampered signature must fail before host authorization, even
			// though the metadata shape still parses.
			calls = 0
			tampered := e
			tampered.Content = "tampered"
			if err := admit(ctx, tampered); err == nil || errors.Is(err, denied) || calls != 0 {
				t.Fatalf("signature stage bypassed: calls=%d err=%v", calls, err)
			}
		}
	}
	var count int
	if err := store.DB().QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected metadata persisted: count=%d err=%v", count, err)
	}
}

func TestMetadataShapePrecedesRepositoryLookup(t *testing.T) {
	// No store or host is needed to reject shape. In particular, HEAD must
	// not be deferred until after resolving the announcement and maintainers.
	g := &GitRelay{}
	for _, tag := range [][]string{{"HEAD", "detached"}, {"refs/heads/bad..name", strings.Repeat("a", 40)}} {
		e := Event{Kind: 30618, Tags: [][]string{{"d", "demo"}, tag}}
		if _, err := g.parseRepository(e); err == nil || !strings.HasPrefix(err.Error(), "invalid:") {
			t.Fatalf("shape error must precede repository lookup: %v", err)
		}
	}
}

func TestInvalidMetadataShapePrecedesHostAdmission(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	calls := 0
	denied := errors.New("host denial")
	g, err := New(Config{Store: store, Root: filepath.Join(t.TempDir(), "git"), Authorize: func(context.Context, Event, Repository) error {
		calls++
		return denied
	}})
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	g.repos[key(owner, "demo")] = Repository{Owner: owner, Identifier: "demo", EventID: "announcement"}
	e := Event{Kind: 30618, CreatedAt: 1, Tags: [][]string{{"d", "demo"}, {"refs/heads/bad..name", strings.Repeat("a", 40)}}}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	for name, admit := range map[string]func(context.Context, Event) error{
		"validate": func(ctx context.Context, e Event) error { _, err := g.Validate(ctx, e); return err },
		"import":   func(ctx context.Context, e Event) error { _, err := g.ValidateImported(ctx, e); return err },
		"publish":  g.Publish,
	} {
		t.Run(name, func(t *testing.T) {
			calls = 0
			if err := admit(ctx, e); err == nil || !strings.HasPrefix(err.Error(), "invalid:") {
				t.Errorf("invalid shape returned %v, want shape error before host denial", err)
			}
			if calls != 0 {
				t.Errorf("host authorizer called %d times for invalid shape", calls)
			}
			var count int
			if err := store.DB().QueryRowContext(ctx, "SELECT count(*) FROM events WHERE id=?", e.ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("invalid shape persisted: count=%d err=%v", count, err)
			}
		})
	}
}

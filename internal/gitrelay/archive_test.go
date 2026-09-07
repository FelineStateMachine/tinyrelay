package gitrelay

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestArchiveAnnouncementRequiresOptInWhenServiceConfigured(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secret := strings.Repeat("0", 63) + "1"
	makeEvent := func() event.Event {
		e := event.Event{Kind: 30617, CreatedAt: 1, Tags: [][]string{{"d", "archive"}}}
		if err := event.Sign(&e, secret); err != nil {
			t.Fatal(err)
		}
		return e
	}
	for name, p := range map[string]policy.Policy{
		"disabled": {},
		"enabled":  {Features: policy.Features{Grasp02: true, Grasp05: true}},
	} {
		t.Run(name, func(t *testing.T) {
			g, err := New(Config{Store: store, Root: filepath.Join(root, name), PublicURL: "https://relay.example", Policy: func() policy.Policy { return p }})
			if err != nil {
				t.Fatal(err)
			}
			_, err = g.Validate(context.Background(), makeEvent())
			if name == "disabled" && err == nil {
				t.Fatal("archive announcement accepted while GRASP-05 is disabled")
			}
			if name == "enabled" && err != nil {
				t.Fatalf("archive announcement rejected: %v", err)
			}
		})
	}
}

func TestArchiveAdmissionRecognizesServiceAndMaintainerExceptions(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ownerSecret := strings.Repeat("0", 63) + "1"
	maintainerSecret := strings.Repeat("0", 63) + "2"
	_, _ = event.PublicKey(ownerSecret)
	maintainer, _ := event.PublicKey(maintainerSecret)
	p := policy.Policy{Features: policy.Features{Grasp02: true, Grasp05: true}}
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git"), PublicURL: "https://relay.example", Policy: func() policy.Policy { return p }, Authorize: func(context.Context, event.Event, Repository) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ownerAnnouncement := event.Event{Kind: 30617, CreatedAt: 1, Tags: [][]string{{"d", "repo"}, {"clone", "https://relay.example/repo.git"}, {"relays", "wss://relay.example"}, {"maintainers", maintainer}}}
	if err := event.Sign(&ownerAnnouncement, ownerSecret); err != nil {
		t.Fatal(err)
	}
	if err := g.Publish(context.Background(), ownerAnnouncement); err != nil {
		t.Fatal(err)
	}
	maintainerAnnouncement := event.Event{Kind: 30617, CreatedAt: 2, Tags: [][]string{{"d", "repo"}}}
	if err := event.Sign(&maintainerAnnouncement, maintainerSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Validate(context.Background(), maintainerAnnouncement); err != nil {
		t.Fatalf("recognized maintainer archive announcement rejected: %v", err)
	}
	wrong := event.Event{Kind: 30617, CreatedAt: 3, Tags: [][]string{{"d", "other"}, {"clone", "https://elsewhere.example/other.git"}, {"relays", "wss://elsewhere.example"}}}
	if err := event.Sign(&wrong, ownerSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Validate(context.Background(), wrong); err == nil {
		t.Fatal("announcement naming another service was accepted")
	}
}

package policy

import (
	"context"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

type privateRepositoryLookup struct {
	private bool
}

func (l privateRepositoryLookup) IsPrivateRepository(context.Context, string, string) (bool, error) {
	return l.private, nil
}

func TestPrivateRepositoryStateInheritsAnnouncement(t *testing.T) {
	announcement := event.Event{Kind: event.KIND_REPO, Tags: [][]string{{"private", "true"}}}
	private, err := PrivateRepository(context.Background(), nil, announcement)
	if err != nil || !private {
		t.Fatalf("announcement privacy = %v, %v; want true, nil", private, err)
	}
	state := event.Event{Kind: event.KIND_REPO_STATE, PubKey: "owner", Tags: [][]string{{"d", "repo"}}}
	private, err = PrivateRepository(context.Background(), privateRepositoryLookup{private: true}, state)
	if err != nil || !private {
		t.Fatalf("stored state privacy = %v, %v; want true, nil", private, err)
	}
	private, err = PrivateRepository(context.Background(), nil, state)
	if err != nil || private {
		t.Fatalf("state without lookup privacy = %v, %v; want false, nil", private, err)
	}
}

func TestPrivatePeerBaseMatchesPathAndNormalizesScheme(t *testing.T) {
	peers := []string{"wss://peer.example/relay/"}
	if got := PrivatePeerBase("https://peer.example/relay/repo.git", peers); got != "https://peer.example/relay" {
		t.Fatalf("matching peer base = %q; want https://peer.example/relay", got)
	}
	if got := PrivatePeerBase("wss://peer.example/relay/repo.git", []string{"https://peer.example/relay"}); got != "https://peer.example/relay" {
		t.Fatalf("reverse websocket peer base = %q; want https://peer.example/relay", got)
	}
	if got := PrivatePeerBase("https://peer.example/other", peers); got != "" {
		t.Fatalf("non-matching peer base = %q; want empty", got)
	}
}

func TestPrivatePeerBaseMatchesRootPeer(t *testing.T) {
	if got := PrivatePeerBase("https://peer.example/repo.git", []string{"https://peer.example"}); got != "https://peer.example" {
		t.Fatalf("root peer base = %q; want https://peer.example", got)
	}
}

func TestPrivateServiceEnabledRequiresCompleteContract(t *testing.T) {
	p := Defaults("owner")
	p.Features.Grasp08 = true
	if p.PrivateServiceEnabled() {
		t.Fatal("GRASP-08 without base GRASP or member reads should be disabled")
	}
	p.Features.Grasp = true
	if p.PrivateServiceEnabled() {
		t.Fatal("GRASP-08 with open reads should be disabled")
	}
	p.Reads = "members"
	if !p.PrivateServiceEnabled() {
		t.Fatal("complete private service contract should be enabled")
	}
}

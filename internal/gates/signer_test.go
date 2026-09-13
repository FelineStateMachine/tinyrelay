package gates

import (
	"context"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
)

func TestSignerRepliesReachUnauthenticatedRecipientSubscription(t *testing.T) {
	sender, recipient := strings.Repeat("a", 64), strings.Repeat("b", 64)
	message := event.Event{Kind: event.KIND_NOSTR_CONNECT, PubKey: sender, Tags: [][]string{{"p", recipient}}}
	for _, tc := range []struct {
		name    string
		enabled bool
		filter  *event.Filter
		session relay.Session
		want    bool
	}{
		{"encrypted reply", true, &event.Filter{Kinds: []int{24133}, Tags: map[string][]string{"p": {recipient}}}, relay.Session{}, true},
		{"unrelated recipient", true, &event.Filter{Kinds: []int{24133}, Tags: map[string][]string{"p": {sender}}}, relay.Session{}, false},
		{"unscoped subscription", true, &event.Filter{Kinds: []int{24133}}, relay.Session{}, false},
		{"authenticated recipient", true, nil, relay.Session{PubKeys: []string{recipient}}, true},
		{"signer disabled", false, &event.Filter{Kinds: []int{24133}, Tags: map[string][]string{"p": {recipient}}}, relay.Session{PubKeys: []string{recipient}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := policy.Defaults(sender)
			p.Reads, p.Writes = "members", "owner"
			p.Features.Signer = tc.enabled
			g, err := New(Config{Policy: func() policy.Policy { return p }})
			if err != nil {
				t.Fatal(err)
			}
			if got := g.CanSee(context.Background(), message, tc.session, tc.filter); got != tc.want {
				t.Fatalf("can see signer reply: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestArtifactVisibilityTracksNestedSourcesAndCycles(t *testing.T) {
	p := policy.Defaults(strings.Repeat("a", 64))
	nodes := map[string]event.Event{
		"a": {ID: "a", Kind: event.KIND_VIEW},
		"b": {ID: "b", Kind: event.KIND_VIEW},
	}
	refs := map[string][]string{"a": {"b"}, "b": {"hidden"}}
	var gate *Gate
	gate, _ = New(Config{Policy: func() policy.Policy { return p }, ArtifactVisible: func(ctx context.Context, e event.Event, s relay.Session) bool {
		for _, id := range refs[e.ID] {
			if id == "hidden" {
				return false
			}
			if !gate.CanSee(ctx, nodes[id], s, nil) {
				return false
			}
		}
		return true
	}})
	if gate.CanSee(context.Background(), nodes["a"], relay.Session{}, nil) {
		t.Fatal("nested artifact with hidden source became visible")
	}
	refs["b"] = []string{"a"}
	if gate.CanSee(context.Background(), nodes["a"], relay.Session{}, nil) {
		t.Fatal("cyclic artifact source became visible")
	}
}

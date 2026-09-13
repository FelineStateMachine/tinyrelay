package gates

import (
	"context"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
)

func TestMembersOnlyReadDistinguishesAnonymousAndAuthenticatedOutsider(t *testing.T) {
	p := policy.Defaults("owner")
	p.Reads = "members"
	g, err := New(Config{Policy: func() policy.Policy { return p }})
	if err != nil {
		t.Fatal(err)
	}
	filter := []event.Filter{{Kinds: []int{1}}}
	if _, err := g.Read(context.Background(), filter, relay.Session{}); err == nil || err.Error() != "auth-required: this relay is members-only" {
		t.Fatalf("anonymous read error = %v", err)
	}
	if _, err := g.Read(context.Background(), filter, relay.Session{PubKeys: []string{"outsider"}}); err == nil || err.Error() != "restricted: this relay is members-only" {
		t.Fatalf("authenticated outsider read error = %v", err)
	}
}

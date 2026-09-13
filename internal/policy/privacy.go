package policy

import (
	"context"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/peerurl"
)

// PrivateRepositoryLookup supplies the stored repository privacy marker for
// replaceable repository state events. It keeps the policy package independent
// from the storage implementation.
type PrivateRepositoryLookup interface {
	IsPrivateRepository(ctx context.Context, pubkey, identifier string) (bool, error)
}

// PrivateServiceEnabled reports whether the tenant has the complete GRASP-08
// private service contract enabled.
func (p Policy) PrivateServiceEnabled() bool {
	return p.Features.Grasp && p.Features.Grasp08 && p.Reads == "members"
}

// PrivateRepository reports whether an event is a private repository event.
// Repository state inherits privacy from its corresponding announcement.
func PrivateRepository(ctx context.Context, lookup PrivateRepositoryLookup, e event.Event) (bool, error) {
	if IsPrivateRepository(e) {
		return true, nil
	}
	if lookup == nil || e.Kind != event.KIND_REPO_STATE {
		return false, nil
	}
	return lookup.IsPrivateRepository(ctx, e.PubKey, event.Tag(e, "d"))
}

// PrivatePeerBase returns the configured HTTP base for a private peer URL.
func PrivatePeerBase(source string, peers []string) string {
	return peerurl.Match(source, peers)
}

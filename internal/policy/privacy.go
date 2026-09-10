package policy

import (
	"context"
	"net/url"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
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

// PrivatePeerBase returns the normalized configured base for a source URL.
// WebSocket schemes are mapped to their HTTP equivalents because private Git
// transport probes and signs HTTP repository roots.
func PrivatePeerBase(source string, peers []string) string {
	u, err := url.Parse(source)
	if err != nil {
		return ""
	}
	for _, configured := range peers {
		peer, err := url.Parse(strings.TrimRight(strings.TrimSpace(configured), "/"))
		if err != nil || normalizePeerScheme(peer.Scheme) != normalizePeerScheme(u.Scheme) || peer.Host != u.Host {
			continue
		}
		base := strings.TrimRight(peer.Path, "/")
		if base != "" && u.Path != base && !strings.HasPrefix(u.Path, base+"/") {
			continue
		}
		peer.Scheme = normalizePeerScheme(peer.Scheme)
		peer.Path = base
		peer.RawPath = ""
		return strings.TrimRight(peer.String(), "/")
	}
	return ""
}

func normalizePeerScheme(scheme string) string {
	switch scheme {
	case "ws":
		return "http"
	case "wss":
		return "https"
	default:
		return scheme
	}
}

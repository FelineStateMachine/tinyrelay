package tinygit

import (
	"context"
	"path/filepath"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// Event is the signed Nostr wire event accepted by the Git engine. Clients
// sign events themselves; the server never needs their private keys.
type Event = event.Event

// Policy configures the embedded engine. It currently shares tinyrelay's
// policy model; DefaultPolicy provides the initial values.
type Policy = policy.Policy

// Store is the SQLite event store used by the engine. This alias preserves
// shared transactions with tinyrelay; it is not a pluggable storage interface.
type Store = storage.Store

// OpenStore opens the shared event schema without starting any relay services.
// The caller owns the returned store and must close it after stopping requests.
func OpenStore(ctx context.Context, path string) (*Store, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return storage.Open(ctx, path)
}

// DefaultPolicy returns the embedded engine's default host policy. Hosting
// admission and private HTTP authorization still require Config callbacks.
func DefaultPolicy(owner string) Policy { return policy.Defaults(owner) }

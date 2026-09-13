package tinygit

import (
	"context"

	"github.com/FelineStateMachine/tinyrelay/internal/gitstore"
	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
)

var _ Store = (*gitstore.Store)(nil)

// Store is the persistence contract required by GitRelay. Implementations may
// use any durable or in-memory representation of Nostr events. The caller
// owns the store; New never closes it.
type Store interface {
	// Query returns matching events by descending timestamp, then ascending ID,
	// excluding expired, hidden and pending events without applying reader ACLs.
	// now is the Unix expiry cutoff. A positive limit caps the result; f.Limit
	// can impose a smaller cap. With neither limit, all matches are returned.
	Query(context.Context, nostr.Filter, int64, int) ([]Event, error)
	// Save durably records e using now for storage timestamps. Duplicate events
	// return ErrDuplicate.
	Save(context.Context, Event, int64) error
	// Latest returns the newest addressable event for kind, pubkey and d.
	// Ties use the lowest event ID. This includes expired, hidden and pending
	// rows so recovery can check durable state independently of read visibility.
	Latest(context.Context, int, string, string) (Event, bool, error)
	// Exists reports whether id is present with the requested kind.
	Exists(context.Context, string, int) (bool, error)
	// Close releases resources owned by the store.
	Close() error
}

// ErrDuplicate indicates that an event was already persisted.
var ErrDuplicate = gitstore.ErrDuplicate

// OpenStore opens the standard SQLite event schema behind the public Store
// contract. The caller owns the returned store and must close it.
func OpenStore(ctx context.Context, path string) (Store, error) { return gitstore.Open(ctx, path) }

// Package tinyrelay provides an embeddable Nostr relay and a standalone server.
// OpenServer owns a durable event store. New uses a host backend so an integrated
// application can commit event projections and required work atomically before
// publication. Both use the same authentication, subscription and synchronization
// transport. Neither constructor starts an HTTP listener.
package tinyrelay

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/protocol/auth"
	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
)

type Session = auth.Session
type SyncItem = nostr.EventRef

// Backend owns admission, persistence and authorized query results. Publish
// must commit required durable work before returning success. A successful
// duplicate returns a reason beginning with "duplicate:" and is not broadcast.
// New borrows the backend; its caller retains lifecycle ownership.
type Backend interface {
	Publish(context.Context, nostr.Event, Session) (string, error)
	Query(context.Context, []nostr.Filter, Session) ([]nostr.Event, error)
	Count(context.Context, []nostr.Filter, Session) (any, error)
}

// Reader applies current access policy to historical and live event delivery.
type Reader interface {
	CanRead(nostr.Event, Session) bool
}

// FilterReader additionally supplies the subscription filter to access policy.
type FilterReader interface {
	CanReadFilter(nostr.Event, Session, *nostr.Filter) bool
}

// QueryHints adds completion and authentication hints to a historical query.
type QueryHints interface {
	QueryHints(context.Context, []nostr.Filter, Session) ([]nostr.Event, []string, error)
}

// PolicyChanges releases host subscription state after access is invalidated.
type PolicyChanges interface {
	CloseSubscriptions(Session)
}

// SyncBackend supplies an authorized snapshot for NIP-77 reconciliation.
type SyncBackend interface {
	Sync(context.Context, nostr.Filter, Session) ([]SyncItem, error)
}

// Config controls connection identity, limits, authorization and observability.
// Zero MaxMessageBytes leaves input unbounded; a negative value selects the
// transport default. OpenServer supplies a bounded standalone default instead.
type Config struct {
	RelayURL        string
	RequestRelayURL func(*http.Request) string
	OnAuthenticate  func(context.Context, Session) error
	MaxMessageBytes int64
	MaxPendingBytes int
	ReadLimit       time.Duration
	WriteLimit      time.Duration
	PingInterval    time.Duration
	OriginPatterns  []string
	OnConnection    func(int)
	OnSubscription  func(int)
	// ChangesAccess identifies host-specific events that invalidate active
	// subscriptions. Standalone relays leave this unset.
	ChangesAccess func(nostr.Event) bool
}

// Relay owns live connections and serializes publication with query snapshots.
// Stop accepting HTTP requests before Close, which drains its connections.
type Relay struct {
	router *relay.Router
}

var _ http.Handler = (*Relay)(nil)

// New embeds the relay protocol engine around a caller-owned backend.
func New(backend Backend, cfg Config) (*Relay, error) {
	if backend == nil {
		return nil, errors.New("tinyrelay: backend is required")
	}
	router := relay.New(backend, relay.Config{
		RelayURL: cfg.RelayURL, RequestRelayURL: cfg.RequestRelayURL,
		OnAuthenticate: cfg.OnAuthenticate, MaxMessageBytes: cfg.MaxMessageBytes,
		MaxPendingBytes: cfg.MaxPendingBytes, ReadLimit: cfg.ReadLimit,
		WriteLimit: cfg.WriteLimit, PingInterval: cfg.PingInterval,
		OriginPatterns: append([]string(nil), cfg.OriginPatterns...),
		OnConnection:   cfg.OnConnection, OnSubscription: cfg.OnSubscription,
		ChangesAccess: cfg.ChangesAccess,
	})
	return &Relay{router: router}, nil
}

// ServeHTTP upgrades a request to the Nostr WebSocket transport.
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.router.HandleHTTP(w, req)
}

// Publish commits through the backend before delivering to live subscribers.
func (r *Relay) Publish(ctx context.Context, e nostr.Event, s Session) (string, error) {
	return r.router.Publish(ctx, e, s)
}

// Listen observes accepted events before per-reader access checks. The callback
// runs under the publication fence and must not block or publish recursively.
// Call the returned function to unregister the observer.
func (r *Relay) Listen(fn func(nostr.Event)) func() { return r.router.Listen(fn) }

// BroadcastGenerated delivers an event that has already been durably accepted.
// Use CommitGenerated when persistence must share the historical query fence.
func (r *Relay) BroadcastGenerated(e nostr.Event) { r.router.BroadcastGenerated(e) }

// CommitGenerated commits an imported/generated event under the publication
// fence, then broadcasts it. Failed commits never reach subscribers.
func (r *Relay) CommitGenerated(ctx context.Context, commit func(context.Context) (nostr.Event, error)) (nostr.Event, error) {
	return r.router.CommitGenerated(ctx, commit)
}

// ApplyACLChange applies an access mutation and invalidates subscriptions while
// holding the same fence used by publication and reconciliation snapshots.
func (r *Relay) ApplyACLChange(ctx context.Context, change func(context.Context) (any, error), reason string) (any, error) {
	return r.router.ApplyACLChange(ctx, change, reason)
}

// CloseSubscriptions invalidates current subscriptions after a policy revision.
func (r *Relay) CloseSubscriptions(reason string) { r.router.CloseSubscriptions(reason) }

// Close stops and joins live connections without closing the borrowed backend.
func (r *Relay) Close(ctx context.Context) error { return r.router.Close(ctx) }

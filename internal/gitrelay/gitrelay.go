// Package gitrelay provides the self-hosted GRASP smart-HTTP boundary. Git
// own receive-pack/upload-pack remain the object and pack implementation;
// signed Nostr repository events remain the authority for refs.
package gitrelay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type Repository struct {
	Owner       string
	Identifier  string
	EventID     string
	Private     bool
	Clone       []string
	Relays      []string
	Maintainers []string
	Refs        map[string]string
	Head        string
	Alternative bool
}

// Maintainer is one key that may sign repository state, push refs and change
// status. Role is "owner", "maintainer" for a key in the announcement's
// maintainers tag, or "agent" for a key the host vouches for under an agent
// grant; Name carries the grant's label when it has one.
type Maintainer struct {
	PubKey string `json:"pubkey"`
	Role   string `json:"role"`
	Name   string `json:"name,omitempty"`
}

// MaintainerSource answers whether a key maintains a repository beyond the
// owner and the announcement's maintainers tag. The host relay supplies it so
// authority it records elsewhere, such as an agent grant, counts in every
// place this package checks maintainers.
type MaintainerSource interface {
	IsMaintainer(ctx context.Context, r Repository, pubkey string) bool
}

type Config struct {
	Store     *storage.Store
	Root      string
	Policy    func() policy.Policy
	Authorize func(context.Context, event.Event, Repository) error
	// Maintainers extends maintainer checks with host-side authority. It is
	// consulted only after the owner and the maintainers tag.
	Maintainers MaintainerSource
	// AuthorizeHTTP is called for private repositories and for operators that
	// want signed/NIP-98 authorization at the Git HTTP boundary.
	AuthorizeHTTP func(context.Context, *http.Request, Repository) error
	// ServiceURL is used to reject recursive GRASP sources and to construct
	// advertised clone URLs. It is optional for an embedded relay.
	ServiceURL string
	// PublicURL is the canonical HTTPS URL used in repository advertisements;
	// ServiceURL remains accepted as a compatibility alias.
	PublicURL string
	// EnableGRASP06 enables the alternative PR repository surface. It is kept
	// opt-in because publishing a capability creates an interoperability
	// promise beyond the base GRASP-01 HTTP endpoint.
	EnableGRASP06 bool
	// AllowMissingObjects is retained for configuration compatibility. Missing
	// objects always enter the durable pending path; this flag no longer makes
	// incomplete state visible.
	AllowMissingObjects bool
	// AllowPrivateRelays permits operator-controlled private DNS targets for
	// self-hosted networks. Public deployments should leave it disabled.
	AllowPrivateRelays bool
	// PrivatePeers are operator-configured GRASP-08 origins. Only these
	// sources may receive signed outbound Git requests.
	PrivatePeers []string
	// HTTPAuth signs requests to configured private peers. It is never used for
	// sources that are merely present in a repository announcement.
	HTTPAuth HTTPAuthSigner
	// GitSync and EventSync are operator-owned transport callbacks. Keeping
	// transport outside this package allows native websocket/NIP-77, HTTP, or
	// an internal queue without making GRASP depend on one client library.
	GitSync   func(context.Context, Repository) error
	EventSync func(context.Context, Repository) error
	// OnPromote is called after a pending state becomes object-complete and
	// visible. The host relay uses it to release event visibility/live fanout.
	OnPromote func(context.Context, string, Repository) error
}

type GitRelay struct {
	store         *storage.Store
	root          string
	policy        func() policy.Policy
	authorize     func(context.Context, event.Event, Repository) error
	authorizeHTTP func(context.Context, *http.Request, Repository) error
	maintainers   MaintainerSource
	serviceURL    string
	gitSync       func(context.Context, Repository) error
	eventSync     func(context.Context, Repository) error
	onPromote     func(context.Context, string, Repository) error
	grasp06       bool
	allowMissing  bool
	allowPrivate  bool
	privatePeers  []string
	httpAuth      HTTPAuthSigner
	mu            sync.RWMutex
	repos         map[string]Repository
	pending       map[string]struct{}
	commitMu      sync.Mutex
	tickMu        sync.Mutex
	repoConfigMu  sync.Mutex
	configured    map[string]struct{}
}

func New(cfg Config) (*GitRelay, error) {
	if cfg.Store == nil {
		return nil, errors.New("git relay: store is required")
	}
	if cfg.Root == "" {
		return nil, errors.New("git relay: root is required")
	}
	if err := os.MkdirAll(cfg.Root, 0700); err != nil {
		return nil, fmt.Errorf("git relay root: %w", err)
	}
	p := cfg.Policy
	if p == nil {
		p = func() policy.Policy { return policy.Defaults("") }
	}
	serviceURL := cfg.PublicURL
	if serviceURL == "" {
		serviceURL = cfg.ServiceURL
	}
	g := &GitRelay{store: cfg.Store, root: cfg.Root, policy: p, authorize: cfg.Authorize, authorizeHTTP: cfg.AuthorizeHTTP, maintainers: cfg.Maintainers, serviceURL: strings.TrimRight(serviceURL, "/"), grasp06: cfg.EnableGRASP06, allowMissing: cfg.AllowMissingObjects, allowPrivate: cfg.AllowPrivateRelays, privatePeers: append([]string(nil), cfg.PrivatePeers...), httpAuth: cfg.HTTPAuth, gitSync: cfg.GitSync, eventSync: cfg.EventSync, onPromote: cfg.OnPromote, repos: make(map[string]Repository), pending: make(map[string]struct{})}
	if err := g.recoverJournals(); err != nil {
		return nil, err
	}
	if err := g.Reload(context.Background()); err != nil {
		return nil, err
	}
	return g, nil
}

func key(owner, identifier string) string { return owner + "\x00" + identifier }

// IsMaintainer reports whether pubkey may sign state and push refs for a
// repository: the owner, a key in the announcement's maintainers tag, or a key
// the configured MaintainerSource vouches for. Every maintainer check in this
// package goes through here so the host's answer applies uniformly.
func (g *GitRelay) IsMaintainer(ctx context.Context, r Repository, pubkey string) bool {
	if pubkey == "" {
		return false
	}
	if pubkey == r.Owner || contains(r.Maintainers, pubkey) {
		return true
	}
	return g.maintainers != nil && g.maintainers.IsMaintainer(ctx, r, pubkey)
}

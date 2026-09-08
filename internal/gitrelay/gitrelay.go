// Package gitrelay provides the self-hosted GRASP smart-HTTP boundary. Git's
// own receive-pack/upload-pack remain the object and pack implementation;
// signed Nostr repository events remain the authority for refs.
package gitrelay

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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

type Config struct {
	Store     *storage.Store
	Root      string
	Policy    func() policy.Policy
	Authorize func(context.Context, event.Event, Repository) error
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

// Service is the durable GRASP scheduler boundary. Tick is safe to call from
// a daemon ticker and runs one pass over the currently accepted repositories.
// Callback progress is persisted so a restart does not lose the last pass.
type Service struct {
	relay *GitRelay
}

// ValidateImported admits metadata received through an explicitly
// synchronized relay. Local announcements continue through Validate.
func (g *GitRelay) ValidateImported(ctx context.Context, e event.Event) (Repository, error) {
	if err := event.Validate(e); err != nil {
		return Repository{}, err
	}
	if e.Kind != 30617 && e.Kind != 30618 {
		return Repository{}, fmt.Errorf("unsupported: event kind %d", e.Kind)
	}
	r, err := g.parseRepository(e)
	if err != nil {
		return Repository{}, err
	}
	if g.authorize != nil {
		if err := g.authorize(ctx, e, r); err != nil {
			return Repository{}, err
		}
	}
	if e.Kind == 30618 {
		if r.EventID == "" {
			return Repository{}, errors.New("blocked: repository announcement is missing")
		}
		if err := g.validateRefs(r.Refs); err != nil {
			return Repository{}, err
		}
	}
	return r, nil
}

const (
	graspRetryDelay   = int64(5 * 60)
	graspSuccessDelay = int64(55 * 60)
)

type tickProgress struct {
	At      int64                     `json:"at"`
	LastKey string                    `json:"last_key"`
	Repos   map[string]tickRepository `json:"repos,omitempty"`
}

type tickRepository struct {
	AttemptedAt int64  `json:"attempted_at"`
	SucceededAt int64  `json:"succeeded_at,omitempty"`
	NextAt      int64  `json:"next_at,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Attempts    int    `json:"attempts"`
	Error       string `json:"error,omitempty"`
}

// RepositorySyncStatus is the durable status of one GRASP reconciliation job.
type RepositorySyncStatus struct {
	AttemptedAt int64  `json:"attempted_at"`
	SucceededAt int64  `json:"succeeded_at,omitempty"`
	NextAt      int64  `json:"next_at,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Attempts    int    `json:"attempts"`
	Error       string `json:"error,omitempty"`
}

// SyncStatus is a read-only snapshot suitable for relay status pages.
type SyncStatus struct {
	At           int64                           `json:"at"`
	LastKey      string                          `json:"last_key"`
	Repositories map[string]RepositorySyncStatus `json:"repositories,omitempty"`
}

func (g *GitRelay) GRASPService() *Service { return &Service{relay: g} }

func (s *Service) progressPath() string {
	return filepath.Join(s.relay.root, ".tinyrelay", "grasp-progress.json")
}

func (s *Service) SyncStatus(ctx context.Context) (SyncStatus, error) {
	if s == nil || s.relay == nil {
		return SyncStatus{}, errors.New("git relay: nil GRASP service")
	}
	select {
	case <-ctx.Done():
		return SyncStatus{}, ctx.Err()
	default:
	}
	b, err := os.ReadFile(s.progressPath())
	if errors.Is(err, os.ErrNotExist) {
		return SyncStatus{Repositories: make(map[string]RepositorySyncStatus)}, nil
	}
	if err != nil {
		return SyncStatus{}, err
	}
	var progress tickProgress
	if err := json.Unmarshal(b, &progress); err != nil {
		return SyncStatus{}, fmt.Errorf("decode GRASP progress: %w", err)
	}
	status := SyncStatus{At: progress.At, LastKey: progress.LastKey, Repositories: make(map[string]RepositorySyncStatus, len(progress.Repos))}
	for key, repo := range progress.Repos {
		status.Repositories[key] = RepositorySyncStatus{AttemptedAt: repo.AttemptedAt, SucceededAt: repo.SucceededAt, NextAt: repo.NextAt, Fingerprint: repo.Fingerprint, Attempts: repo.Attempts, Error: repo.Error}
	}
	return status, nil
}

func (s *Service) Tick(ctx context.Context) error {
	if s == nil || s.relay == nil {
		return errors.New("git relay: nil GRASP service")
	}
	s.relay.tickMu.Lock()
	defer s.relay.tickMu.Unlock()
	s.relay.mu.RLock()
	repos := make([]Repository, 0, len(s.relay.repos))
	for _, r := range s.relay.repos {
		repos = append(repos, r)
	}
	s.relay.mu.RUnlock()
	sort.Slice(repos, func(i, j int) bool {
		return key(repos[i].Owner, repos[i].Identifier) < key(repos[j].Owner, repos[j].Identifier)
	})
	var last string
	progress := tickProgress{At: time.Now().Unix(), Repos: make(map[string]tickRepository, len(repos))}
	previousProgress, _ := s.loadProgress()
	// Resume after the last attempted repository if the prior pass ran out
	// of time. Preserve untouched statuses instead of marking them failed.
	last = previousProgress.LastKey
	for _, r := range repos {
		if previous, ok := previousProgress.Repos[key(r.Owner, r.Identifier)]; ok {
			progress.Repos[key(r.Owner, r.Identifier)] = previous
		}
	}
	start := sort.Search(len(repos), func(i int) bool { return key(repos[i].Owner, repos[i].Identifier) > last })
	repos = append(repos[start:], repos[:start]...)
	var tickErr error
	for _, r := range repos {
		if ctx.Err() != nil {
			tickErr = errors.Join(tickErr, ctx.Err())
			break
		}
		repoKey := key(r.Owner, r.Identifier)
		status := tickRepository{AttemptedAt: progress.At, Attempts: 1}
		profile := s.relay.policy()
		status.Fingerprint = repoFingerprint(r, profile)
		if previous, ok := previousProgress.Repos[repoKey]; ok {
			if previous.NextAt > progress.At && previous.Fingerprint == status.Fingerprint {
				progress.Repos[repoKey] = previous
				continue
			}
			if previous.Error != "" {
				status.Attempts = previous.Attempts + 1
			}
		}
		last = repoKey
		features := profile.Features
		var repoErr error
		if features.Grasp && features.Grasp02 {
			if s.relay.eventSync != nil {
				eventCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
				repoErr = errors.Join(repoErr, s.relay.eventSync(eventCtx, r))
				cancel()
			}
			gitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			if s.relay.gitSync != nil {
				repoErr = errors.Join(repoErr, s.relay.gitSync(gitCtx, r))
			} else if len(r.Refs) > 0 && len(RepairSources(r)) > 0 {
				repoErr = errors.Join(repoErr, s.relay.FetchMissing(gitCtx, r, RepairSources(r), r.Refs))
			}
			cancel()
		}
		if repoErr != nil {
			status.Error = repoErr.Error()
			status.NextAt = progress.At + graspRetryDelay
			tickErr = errors.Join(tickErr, fmt.Errorf("%s synchronization: %w", last, repoErr))
		} else {
			status.SucceededAt = progress.At
			status.NextAt = progress.At + graspSuccessDelay
		}
		progress.Repos[last] = status
	}
	progress.LastKey = last
	b, err := json.Marshal(progress)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.progressPath()), 0700); err != nil {
		return err
	}
	tmp := s.progressPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.progressPath()); err != nil {
		return err
	}
	return tickErr
}

func repoFingerprint(r Repository, profile policy.Policy) string {
	refs := make([]string, 0, len(r.Refs))
	for ref, oid := range r.Refs {
		refs = append(refs, ref+"="+oid)
	}
	sort.Strings(refs)
	profileJSON, _ := json.Marshal(struct {
		Grasp   bool     `json:"grasp"`
		Reads   string   `json:"reads"`
		Grasp02 bool     `json:"grasp02"`
		Grasp03 bool     `json:"grasp03"`
		Peers   []string `json:"peers"`
	}{profile.Features.Grasp, profile.Reads, profile.Features.Grasp02, profile.Features.Grasp03, profile.PrivatePeers})
	parts := []string{r.Owner, r.Identifier, r.EventID, r.Head, strconv.FormatBool(r.Private), strings.Join(refs, "\x00"), strings.Join(r.Clone, "\x00"), strings.Join(r.Relays, "\x00"), strings.Join(r.Maintainers, "\x00"), string(profileJSON)}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x01")))
	return hex.EncodeToString(sum[:])
}

func (s *Service) loadProgress() (tickProgress, error) {
	b, err := os.ReadFile(s.progressPath())
	if err != nil {
		return tickProgress{}, err
	}
	var progress tickProgress
	if err := json.Unmarshal(b, &progress); err != nil {
		return tickProgress{}, err
	}
	return progress, nil
}

// RepairSources gives a daemon a deterministic source order for a missing
// object. Clone URLs are preferred, followed by relay URLs; callers must still
// pass each URL through AdmitSource and verify the object graph.
func RepairSources(r Repository) []string {
	out := append([]string(nil), r.Clone...)
	out = append(out, r.Relays...)
	return out
}

type journalRecord struct {
	Repository string            `json:"repository"`
	EventID    string            `json:"event_id"`
	Kind       int               `json:"kind"`
	Refs       map[string]string `json:"refs,omitempty"`
	Head       string            `json:"head,omitempty"`
	Committed  bool              `json:"committed"`
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
	g := &GitRelay{store: cfg.Store, root: cfg.Root, policy: p, authorize: cfg.Authorize, authorizeHTTP: cfg.AuthorizeHTTP, serviceURL: strings.TrimRight(serviceURL, "/"), grasp06: cfg.EnableGRASP06, allowMissing: cfg.AllowMissingObjects, allowPrivate: cfg.AllowPrivateRelays, privatePeers: append([]string(nil), cfg.PrivatePeers...), httpAuth: cfg.HTTPAuth, gitSync: cfg.GitSync, eventSync: cfg.EventSync, onPromote: cfg.OnPromote, repos: make(map[string]Repository), pending: make(map[string]struct{})}
	if err := g.recoverJournals(); err != nil {
		return nil, err
	}
	if err := g.Reload(context.Background()); err != nil {
		return nil, err
	}
	return g, nil
}

func key(owner, identifier string) string { return owner + "\x00" + identifier }

func (g *GitRelay) journalDir() string { return filepath.Join(g.root, ".tinyrelay", "journal") }

func (g *GitRelay) journalPath(r journalRecord) string {
	h := sha256.Sum256([]byte(r.Repository + "\x00" + r.EventID))
	return filepath.Join(g.journalDir(), hex.EncodeToString(h[:])+".json")
}

func (g *GitRelay) writeJournal(repo Repository, r journalRecord) error {
	if err := os.MkdirAll(g.journalDir(), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := g.journalPath(r) + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, g.journalPath(r))
}

func (g *GitRelay) commitJournal(repo Repository, r journalRecord) error {
	if err := os.Remove(g.journalPath(r)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// recoverJournals completes state publication after a process crash. A
// journal is deliberately small and repository-local; the event store remains
// authoritative for event durability, while the file is the Git authority.
func (g *GitRelay) recoverJournals() error {
	entries, err := os.ReadDir(g.journalDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(g.journalDir(), entry.Name()))
		if err != nil {
			return err
		}
		var jr journalRecord
		if err := json.Unmarshal(b, &jr); err != nil {
			return fmt.Errorf("git relay journal %s: %w", entry.Name(), err)
		}
		owner, id, ok := strings.Cut(jr.Repository, "\x00")
		if !ok || owner == "" || id == "" {
			return fmt.Errorf("git relay journal %s: invalid repository", entry.Name())
		}
		r := Repository{Owner: owner, Identifier: id, Refs: jr.Refs, Head: jr.Head}
		if err := g.ensureRepo(r); err != nil {
			return err
		}
		keepJournal := false
		if jr.Kind == 30618 {
			var current string
			err := g.store.DB().QueryRowContext(context.Background(), `SELECT events.id FROM events JOIN tags ON tags.event_id=events.id AND tags.name='d' AND tags.value=? WHERE events.pubkey=? AND events.kind=30618 ORDER BY events.created_at DESC,events.id ASC LIMIT 1`, id, owner).Scan(&current)
			if errors.Is(err, sql.ErrNoRows) || current != jr.EventID {
				if removeErr := os.Remove(filepath.Join(g.journalDir(), entry.Name())); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					return removeErr
				}
				continue
			}
			if err != nil {
				return err
			}
			if g.stateObjectsPresent(context.Background(), r) {
				err = g.writeState(r)
			} else {
				err = g.writePendingState(r)
				keepJournal = true
			}
			if err != nil {
				return err
			}
			g.mu.Lock()
			g.pending[jr.EventID] = struct{}{}
			g.mu.Unlock()
		} else if jr.Kind == 1617 || jr.Kind == 1618 || jr.Kind == 1619 {
			if !g.stateObjectsPresent(context.Background(), r) {
				if err := g.writePendingState(r); err != nil {
					return err
				}
				keepJournal = true
				g.mu.Lock()
				g.pending[jr.EventID] = struct{}{}
				g.mu.Unlock()
			}
		}
		if !keepJournal {
			if err := os.Remove(filepath.Join(g.journalDir(), entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// Publish ingests a signed repository announcement or state event. Callers
// must validate the event before invoking this method; ValidateEvent is
// provided for callers that want one explicit boundary.
func (g *GitRelay) ValidateEvent(e event.Event) error { return event.Validate(e) }

// Validate performs GRASP admission and returns the repository authority
// that the caller must associate with its event transaction. It has no store
// side effects.
func (g *GitRelay) Validate(ctx context.Context, e event.Event) (Repository, error) {
	if err := event.Validate(e); err != nil {
		return Repository{}, err
	}
	if e.Kind != 30617 && e.Kind != 30618 {
		return Repository{}, fmt.Errorf("unsupported: event kind %d is not GRASP repository metadata", e.Kind)
	}
	r, err := g.parseRepository(e)
	if err != nil {
		return Repository{}, err
	}
	if e.Kind == 30617 {
		if err := g.validateArchiveAnnouncement(r); err != nil {
			return Repository{}, err
		}
	}
	if g.authorize != nil {
		if err := g.authorize(ctx, e, r); err != nil {
			return Repository{}, err
		}
	} else if e.PubKey != r.Owner && !contains(r.Maintainers, e.PubKey) {
		return Repository{}, errors.New("blocked: repository metadata must be signed by owner")
	}
	if e.Kind == 30618 {
		if r.EventID == "" {
			return Repository{}, errors.New("blocked: repository announcement is missing")
		}
		if err := g.validateRefs(r.Refs); err != nil {
			return Repository{}, err
		}
		// Missing objects are a durable pending transition. The signed state is
		// accepted, but remains invisible until receive-pack supplies every tip.
	}
	return r, nil
}

// CommitAfterStore applies the Git-side transition after the caller has
// durably saved the event. It intentionally does not call Store.Save.
func (g *GitRelay) CommitAfterStore(ctx context.Context, e event.Event, repo Repository) error {
	return g.commitAfterStore(ctx, e, repo, true)
}

// CommitAfterStoreNoNotify stages the Git transition without invoking the
// host fanout callback. Use this while already inside the relay publish
// fence; the durable work intent will release visibility afterward.
func (g *GitRelay) CommitAfterStoreNoNotify(ctx context.Context, e event.Event, repo Repository) error {
	return g.commitAfterStore(ctx, e, repo, false)
}

func (g *GitRelay) commitAfterStore(ctx context.Context, e event.Event, repo Repository, notify bool) error {
	g.commitMu.Lock()
	err := g.stageAfterStore(ctx, e, repo)
	pending := g.IsPending(e.ID)
	g.commitMu.Unlock()
	// The host callback enters its publish fence. Never hold the Git lock
	// here: synchronous publication already enters these locks in reverse.
	if err == nil && !pending && notify && g.onPromote != nil {
		return g.onPromote(ctx, e.ID, repo)
	}
	return err
}

func (g *GitRelay) stageAfterStore(ctx context.Context, e event.Event, repo Repository) error {
	current, err := g.currentMetadata(ctx, e)
	if err != nil {
		return err
	}
	if !current {
		// A queued intent may outlive a newer addressable announcement/state.
		// Never let that stale intent replace the currently authorized hook.
		return nil
	}
	if e.Kind == 30617 {
		// Announcements and ref state are separate addressable events. An
		// announcement update must retain the current signed refs and HEAD.
		var raw string
		err := g.store.DB().QueryRowContext(ctx, `SELECT raw FROM events WHERE kind=30618 AND pubkey=? AND d=? ORDER BY created_at DESC,id ASC LIMIT 1`, e.PubKey, event.Tag(e, "d")).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			state, err := event.Parse([]byte(raw))
			if err != nil {
				return err
			}
			stateRepo, err := g.parseRepository(state)
			if err != nil {
				return err
			}
			repo.Refs, repo.Head, repo.EventID = stateRepo.Refs, stateRepo.Head, state.ID
		}
	}
	jr := journalRecord{Repository: key(repo.Owner, repo.Identifier), EventID: e.ID, Kind: e.Kind, Refs: repo.Refs, Head: repo.Head}
	if err := g.writeJournal(repo, jr); err != nil {
		return err
	}
	g.mu.Lock()
	g.repos[key(repo.Owner, repo.Identifier)] = repo
	g.mu.Unlock()
	if err := g.ensureRepo(repo); err != nil {
		return err
	}
	pending := false
	if e.Kind == 30618 {
		var err error
		if g.stateObjectsPresent(ctx, repo) {
			err = g.writeState(repo)
		} else {
			err = g.writePendingState(repo)
			g.mu.Lock()
			g.pending[e.ID] = struct{}{}
			g.mu.Unlock()
			pending = true
		}
		if err != nil {
			return err
		}
	}
	if pending {
		return nil
	}
	if err := g.commitJournal(repo, jr); err != nil {
		return err
	}
	return nil
}

func (g *GitRelay) currentMetadata(ctx context.Context, e event.Event) (bool, error) {
	if e.Kind != 30617 && e.Kind != 30618 {
		return true, nil
	}
	var id string
	err := g.store.DB().QueryRowContext(ctx, `SELECT events.id FROM events JOIN tags ON tags.event_id=events.id AND tags.name='d' AND tags.value=? WHERE events.pubkey=? AND events.kind=? ORDER BY events.created_at DESC,events.id ASC LIMIT 1`, event.Tag(e, "d"), e.PubKey, e.Kind).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return id == e.ID, nil
}

// IsPending lets the event admission/read layer hide a metadata event until
// its signed Git tips are verified locally.
func (g *GitRelay) IsPending(eventID string) bool {
	g.mu.RLock()
	_, ok := g.pending[eventID]
	g.mu.RUnlock()
	return ok
}

// ExpirePending removes the file and in-memory authorities for state events
// whose retained deadline has elapsed. The SQL event sweep calls this before
// deleting the corresponding event so a stale journal cannot promote it after
// a restart.
func (g *GitRelay) ExpirePending(ctx context.Context, eventIDs []string) error {
	if g == nil || len(eventIDs) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(eventIDs))
	for _, id := range eventIDs {
		want[id] = struct{}{}
	}
	entries, err := os.ReadDir(g.journalDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(g.journalDir(), entry.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var jr journalRecord
		if err := json.Unmarshal(body, &jr); err != nil {
			return err
		}
		if _, ok := want[jr.EventID]; !ok {
			continue
		}
		owner, identifier, ok := strings.Cut(jr.Repository, "\x00")
		if ok {
			_ = os.Remove(g.pendingStatePath(Repository{Owner: owner, Identifier: identifier}))
			_ = os.Remove(g.pendingHeadPath(Repository{Owner: owner, Identifier: identifier}))
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	g.mu.Lock()
	for id := range want {
		delete(g.pending, id)
	}
	g.mu.Unlock()
	return ctx.Err()
}

// PruneRef removes an expired native Git ref before its retained metadata is
// deleted. repository may be the storage key or its identifier.
func (g *GitRelay) PruneRef(ctx context.Context, repository, ref string) error {
	if g == nil || !validRef(ref) {
		return errors.New("git relay: invalid expired ref")
	}
	g.mu.RLock()
	var found *Repository
	for _, candidate := range g.repos {
		if key(candidate.Owner, candidate.Identifier) == repository || candidate.Identifier == repository {
			copy := candidate
			found = &copy
			break
		}
	}
	g.mu.RUnlock()
	if found == nil {
		return nil
	}
	if err := exec.CommandContext(ctx, "git", "--git-dir", g.repoPath(*found), "update-ref", "-d", ref).Run(); err != nil {
		return err
	}
	g.mu.Lock()
	if current, ok := g.repos[key(found.Owner, found.Identifier)]; ok {
		delete(current.Refs, ref)
		g.repos[key(found.Owner, found.Identifier)] = current
	}
	g.mu.Unlock()
	return nil
}

// PromotePending verifies and exposes a previously accepted state event. It
// is useful for local receive-pack integrations; ServeHTTP invokes it after a
// smart push as well.
func (g *GitRelay) PromotePending(ctx context.Context, repo Repository) error {
	g.commitMu.Lock()
	promoted, err := g.promotePending(ctx, repo)
	g.commitMu.Unlock()
	if err == nil && promoted && g.onPromote != nil {
		return g.onPromote(ctx, repo.EventID, repo)
	}
	return err
}

func (g *GitRelay) promotePending(ctx context.Context, repo Repository) (bool, error) {
	var current int
	if err := g.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE id=? AND kind=30618`, repo.EventID).Scan(&current); err != nil {
		return false, err
	}
	if current == 0 {
		return false, nil
	}
	if !g.stateObjectsPresent(ctx, repo) {
		return false, nil
	}
	g.mu.RLock()
	_, tracked := g.pending[repo.EventID]
	g.mu.RUnlock()
	if _, err := os.Stat(g.pendingStatePath(repo)); err != nil && !tracked {
		return false, nil
	}
	if err := g.writeState(repo); err != nil {
		return false, err
	}
	_ = os.Remove(g.pendingStatePath(repo))
	_ = os.Remove(g.pendingHeadPath(repo))
	g.mu.Lock()
	delete(g.pending, repo.EventID)
	g.mu.Unlock()
	jr := journalRecord{Repository: key(repo.Owner, repo.Identifier), EventID: repo.EventID, Kind: 30618, Refs: repo.Refs, Head: repo.Head}
	if err := g.commitJournal(repo, jr); err != nil {
		return false, err
	}
	return true, nil
}

func (g *GitRelay) Publish(ctx context.Context, e event.Event) error {
	if err := event.Validate(e); err != nil {
		return err
	}
	if e.Kind != 30617 && e.Kind != 30618 {
		if !((g.grasp06 || g.policy().Features.Grasp06) && (e.Kind == 1617 || e.Kind == 1618 || e.Kind == 1619)) {
			return fmt.Errorf("unsupported: event kind %d is not GRASP repository metadata", e.Kind)
		}
		g.commitMu.Lock()
		defer g.commitMu.Unlock()
		return g.publishPR(ctx, e)
	}
	repo, err := g.Validate(ctx, e)
	if err != nil {
		return err
	}
	if _, err := g.store.Save(ctx, e, storage.SaveOptions{Now: time.Now().Unix()}); err != nil {
		return err
	}
	return g.CommitAfterStore(ctx, e, repo)
}

func (g *GitRelay) parseRepository(e event.Event) (Repository, error) {
	id := event.Tag(e, "d")
	if !validIdentifier(id) {
		return Repository{}, errors.New("invalid: repository identifier")
	}
	r := Repository{Owner: e.PubKey, Identifier: id, EventID: e.ID, Refs: map[string]string{}}
	if e.Kind == 30617 {
		for _, tag := range e.Tags {
			if len(tag) < 2 {
				continue
			}
			switch tag[0] {
			case "private":
				r.Private = tag[1] == "true"
			case "clone":
				r.Clone = append(r.Clone, tag[1:]...)
			case "relays":
				r.Relays = append(r.Relays, tag[1:]...)
			case "maintainers":
				r.Maintainers = append(r.Maintainers, tag[1:]...)
			}
		}
		return r, nil
	}
	// State is replaceable: bind it to the current announcement owner and
	// carry the announcement's identity into the hook/state file.
	state := Repository{Owner: e.PubKey, Identifier: id, EventID: e.ID, Refs: map[string]string{}}
	for _, tag := range e.Tags {
		if len(tag) == 0 {
			continue
		}
		if tag[0] == "HEAD" {
			if len(tag) != 2 || state.Head != "" {
				return Repository{}, errors.New("invalid: repository state has multiple HEAD tags")
			}
			state.Head = tag[1]
			continue
		}
		if len(tag) < 2 {
			continue
		}
		if strings.HasPrefix(tag[0], "refs/") {
			if _, duplicate := state.Refs[tag[0]]; duplicate || tag[1] == "" || !isObjectID(tag[1]) || strings.Trim(tag[1], "0") == "" {
				return Repository{}, fmt.Errorf("invalid: ref %s is not a SHA-1", tag[0])
			}
			state.Refs[tag[0]] = tag[1]
		}
	}
	g.mu.RLock()
	ann := g.repos[key(e.PubKey, id)]
	g.mu.RUnlock()
	if ann.Owner == "" {
		q, err := g.store.Query(context.Background(), event.Filter{Kinds: []int{30617}, Tags: map[string][]string{"d": {id}}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 0})
		if err != nil {
			return Repository{}, err
		}
		if len(q.Events) == 0 {
			return Repository{}, errors.New("blocked: repository announcement is missing")
		}
		for _, candidate := range q.Events {
			ann, err = g.parseRepository(candidate)
			if err == nil && ann.Owner == e.PubKey || err == nil && contains(ann.Maintainers, e.PubKey) {
				break
			}
			ann = Repository{}
		}
		if ann.Owner == "" {
			return Repository{}, errors.New("blocked: state author is not an accepted repository maintainer")
		}
	}
	if e.PubKey != ann.Owner && !contains(ann.Maintainers, e.PubKey) {
		return Repository{}, errors.New("blocked: state author is not an accepted repository maintainer")
	}
	state.Owner, state.Private = ann.Owner, ann.Private
	state.Clone = append([]string(nil), ann.Clone...)
	state.Relays = append([]string(nil), ann.Relays...)
	state.Maintainers = append([]string(nil), ann.Maintainers...)
	state.EventID = e.ID
	if err := validateHead(state.Head, state.Refs); err != nil {
		return Repository{}, err
	}
	return state, nil
}

func validateHead(head string, refs map[string]string) error {
	if head == "" {
		return nil
	}
	if !strings.HasPrefix(head, "ref: ") {
		return errors.New("invalid: HEAD must use ref: syntax")
	}
	ref := strings.TrimPrefix(head, "ref: ")
	if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		return errors.New("invalid: HEAD must name a branch or tag")
	}
	if !validRef(ref) {
		return errors.New("invalid: HEAD ref")
	}
	return nil
}

func (g *GitRelay) validateRefs(refs map[string]string) error {
	for ref, oid := range refs {
		if !validRef(ref) || oid != "" && !isObjectID(oid) {
			return fmt.Errorf("invalid: ref %s", ref)
		}
	}
	return nil
}

// Capabilities returns only protocol surfaces that this instance can serve.
func (g *GitRelay) Capabilities() []string {
	p := g.policy()
	c := []string{"GRASP-01"}
	if p.Features.Grasp02 && g.eventSync != nil {
		c = append(c, "GRASP-02")
		if p.Features.Grasp03 {
			c = append(c, "GRASP-03")
		}
	}
	if g.grasp06 || p.Features.Grasp06 {
		c = append(c, "GRASP-06")
	}
	if p.Features.Grasp && p.Features.Grasp08 && p.Reads == "members" {
		c = append(c, "GRASP-08")
	}
	return c
}

// SupportedGRASPs is the NIP-11-facing capability list. It deliberately
// mirrors Capabilities so callers cannot advertise preview protocols by hand.
func (g *GitRelay) SupportedGRASPs() []string { return g.Capabilities() }

// DeletePRRef removes a temporary GRASP-06 ref before its Nostr metadata is
// deleted. Identity accepts the source coordinates used by delivery jobs:
// pr:<owner>:<identifier> or 30617:<owner>:<identifier>.
func (g *GitRelay) DeletePRRef(ctx context.Context, identity, ref string) error {
	parts := strings.SplitN(identity, ":", 3)
	if len(parts) != 3 || (parts[0] != "pr" && parts[0] != "30617") || !isPubKey(parts[1]) || parts[2] == "" || strings.ContainsAny(parts[2], `/\\`) {
		return errors.New("invalid: PR repository identity")
	}
	if !strings.HasPrefix(ref, "refs/nostr/") || len(strings.TrimPrefix(ref, "refs/nostr/")) != 64 {
		return errors.New("invalid: PR ref")
	}
	r := Repository{Owner: parts[1], Identifier: parts[2], Alternative: true, Refs: map[string]string{}}
	if err := g.ensureRepo(r); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir", g.repoPath(r), "update-ref", "-d", ref)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("delete PR ref: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return g.removeRefFromPending(r, ref)
}

func (g *GitRelay) removeRefFromPending(r Repository, ref string) error {
	b, err := os.ReadFile(g.pendingStatePath(r))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var kept map[string]string = map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] != ref {
			kept[fields[0]] = fields[1]
		}
	}
	if len(kept) == 0 {
		return os.Remove(g.pendingStatePath(r))
	}
	return g.writeRefsFile(g.pendingStatePath(r), kept)
}

// Reload rebuilds repository and pending indexes after a restore has inserted
// events and native Git repositories on disk.
func (g *GitRelay) Reload(ctx context.Context) error {
	g.commitMu.Lock()
	defer g.commitMu.Unlock()
	g.repoConfigMu.Lock()
	g.configured = nil
	g.repoConfigMu.Unlock()
	if err := g.recoverJournals(); err != nil {
		return err
	}
	q, err := g.store.Query(ctx, event.Filter{Kinds: []int{30617}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 0})
	if err != nil {
		return err
	}
	repos := make(map[string]Repository)
	for _, raw := range q.Events {
		r, parseErr := g.parseRepository(raw)
		if parseErr == nil {
			repos[key(r.Owner, r.Identifier)] = r
		}
	}
	// Recover only native refs that match the published signed authority.
	// Incoming receive-pack refs may still be awaiting promotion.
	for coordinate, announcement := range repos {
		if err := g.ensureRepo(announcement); err != nil {
			return err
		}
		refs, refsErr := g.publishedNativeRefs(announcement)
		if refsErr != nil {
			return refsErr
		}
		if len(refs) > 0 {
			announcement.Refs = refs
			if head, headErr := exec.Command("git", "--git-dir", g.repoPath(announcement), "symbolic-ref", "-q", "HEAD").Output(); headErr == nil {
				announcement.Head = "ref: " + strings.TrimSpace(string(head))
			}
			repos[coordinate] = announcement
		}
	}
	// If no native refs exist (for example an older store written before native
	// projection), recover the newest complete signed state as a fallback.
	for coordinate, announcement := range repos {
		if _, err := os.Stat(g.statePath(announcement)); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err := os.Stat(g.pendingStatePath(announcement)); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		states, stateErr := g.store.Query(ctx, event.Filter{Authors: append([]string{announcement.Owner}, announcement.Maintainers...), Kinds: []int{30618}, Tags: map[string][]string{"d": {announcement.Identifier}}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 0})
		if stateErr != nil {
			return stateErr
		}
		if len(states.Events) == 0 {
			continue
		}
		sort.Slice(states.Events, func(i, j int) bool { return states.Events[i].CreatedAt > states.Events[j].CreatedAt })
		state, parseErr := g.parseRepository(states.Events[0])
		if parseErr != nil || state.Owner != announcement.Owner || !g.stateObjectsPresent(ctx, state) {
			continue
		}
		repos[coordinate] = state
	}
	prs, prErr := g.store.Query(ctx, event.Filter{Kinds: []int{1617, 1618, 1619}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 0})
	if prErr != nil {
		return prErr
	}
	for _, raw := range prs.Events {
		identifier := event.Tag(raw, "d")
		if identifier == "" {
			identifier = event.Tag(raw, "a")
		}
		commit := event.Tag(raw, "c")
		if !validIdentifier(identifier) || !isObjectID(commit) {
			continue
		}
		pr := Repository{Owner: raw.PubKey, Identifier: identifier, EventID: raw.ID, Alternative: true, Refs: map[string]string{}}
		if _, err := os.Stat(g.repoPath(pr)); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if refs, refsErr := g.publishedNativeRefs(pr); refsErr == nil && len(refs) > 0 {
			pr.Refs = refs
		}
		repos[key(pr.Owner, pr.Identifier)] = pr
	}
	entries, err := os.ReadDir(g.journalDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	refreshedPending := make(map[string]struct{})
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		b, readErr := os.ReadFile(filepath.Join(g.journalDir(), entry.Name()))
		if readErr != nil {
			return readErr
		}
		var jr journalRecord
		if json.Unmarshal(b, &jr) == nil && jr.EventID != "" {
			refreshedPending[jr.EventID] = struct{}{}
		}
	}
	g.mu.Lock()
	g.repos = repos
	g.pending = refreshedPending
	g.mu.Unlock()
	return nil
}

func (g *GitRelay) publishPR(ctx context.Context, e event.Event) error {
	id := event.Tag(e, "d")
	if id == "" {
		id = event.Tag(e, "a")
	}
	if !validIdentifier(id) {
		return errors.New("invalid: PR repository identifier")
	}
	r := Repository{Owner: e.PubKey, Identifier: id, EventID: e.ID, Alternative: true, Refs: map[string]string{}}
	commit := event.Tag(e, "c")
	if commit != "" && !isObjectID(commit) {
		return errors.New("invalid: PR commit is not a SHA-1")
	}
	if commit != "" {
		r.Refs["refs/nostr/"+e.ID] = commit
	}
	if g.authorize != nil {
		if err := g.authorize(ctx, e, r); err != nil {
			return err
		}
	}
	if err := g.writeJournal(r, journalRecord{Repository: key(r.Owner, r.Identifier), EventID: e.ID, Kind: e.Kind, Refs: r.Refs, Head: r.Head}); err != nil {
		return err
	}
	if _, err := g.store.Save(ctx, e, storage.SaveOptions{Now: time.Now().Unix()}); err != nil {
		return err
	}
	if err := g.ensureRepo(r); err != nil {
		return err
	}
	g.mu.Lock()
	g.repos[key(r.Owner, r.Identifier)] = r
	g.mu.Unlock()
	pending := !g.stateObjectsPresent(ctx, r)
	if !pending {
		if err := g.writeState(r); err != nil {
			return err
		}
	} else {
		if err := g.writePendingState(r); err != nil {
			return err
		}
		g.mu.Lock()
		g.pending[e.ID] = struct{}{}
		g.mu.Unlock()
	}
	if pending {
		return nil
	}
	return g.commitJournal(r, journalRecord{Repository: key(r.Owner, r.Identifier), EventID: e.ID, Kind: e.Kind, Refs: r.Refs, Head: r.Head})
}

// AdmitSource enforces the source restrictions from GRASP's Git sync model.
// Only HTTPS sources are accepted; localhost, private IPs, credentials,
// query/fragment data, and this relay itself are rejected.
func (g *GitRelay) AdmitSource(raw string) (string, error) {
	return g.admitSource(context.Background(), raw, false)
}

func (g *GitRelay) admitSource(ctx context.Context, raw string, resolve bool) (string, error) {
	u, err := url.Parse(raw)
	privatePeer := g.privatePeer(raw)
	if err != nil || (u.Scheme != "https" && !(privatePeer && u.Scheme == "http")) || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid: Git source must be a credential-free HTTPS URL")
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "metadata.google.internal" {
		return "", errors.New("blocked: Git source host is local or address-like")
	}
	if ip := net.ParseIP(host); ip != nil && !g.allowPrivate {
		return "", errors.New("blocked: Git source host is an IP literal")
	}
	if strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
		return "", errors.New("blocked: Git source host is private")
	}
	if resolve {
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return "", fmt.Errorf("Git source DNS lookup: %w", err)
		}
		for _, ip := range ips {
			if privateIP(ip) && !g.allowPrivate {
				return "", errors.New("blocked: Git source resolves to a private address")
			}
		}
	}
	if g.serviceURL != "" && strings.TrimRight(raw, "/") == g.serviceURL {
		return "", errors.New("blocked: Git source points to this relay")
	}
	return strings.TrimRight(raw, "/"), nil
}

func (g *GitRelay) resolvedSource(ctx context.Context, raw string) (string, string, error) {
	source, err := g.admitSource(ctx, raw, false)
	if err != nil {
		return "", "", err
	}
	u, err := url.Parse(source)
	if err != nil {
		return "", "", err
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", u.Hostname())
	if err != nil {
		return "", "", fmt.Errorf("Git source DNS lookup: %w", err)
	}
	for _, ip := range ips {
		if !privateIP(ip) || g.allowPrivate {
			return source, ip.String(), nil
		}
	}
	return "", "", errors.New("blocked: Git source resolves to a private address")
}

func privateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	return ip.IsUnspecified()
}

// FetchMissing downloads only the expected refs from an admitted source. Git
// verifies pack checksums and object connectivity; refs are installed only
// after every expected object exists locally.
func (g *GitRelay) FetchMissing(ctx context.Context, r Repository, sources []string, expected map[string]string) error {
	return g.fetchMissing(ctx, r, sources, expected, 0, 0)
}

// fetchMissing is shared by ordinary branch repair and the more restrictive
// PR repair path. A non-zero depth keeps an untrusted PR clone from making the
// hosted repository walk arbitrary history. maxBytes bounds transfer traffic
// across all sources and the isolated destination after each fetch. These
// limits apply only to isolated repositories, never the hosted branch repo.
func (g *GitRelay) fetchMissing(ctx context.Context, r Repository, sources []string, expected map[string]string, depth int, maxBytes int64) error {
	if err := g.ensureRepo(r); err != nil {
		return err
	}
	local := r
	local.Refs = expected
	if g.stateObjectsPresent(ctx, local) {
		return g.installFetchedRefs(r, expected)
	}
	missing := make(map[string]struct{}, len(expected))
	var budget *byteBudget
	if maxBytes > 0 {
		budget = &byteBudget{}
	}
	var lastErr error
	for ref, oid := range expected {
		if oid != "" {
			missing[ref] = struct{}{}
		}
	}
	for _, raw := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		if budget != nil && len(missing) == 0 {
			break
		}
		if budget != nil && budget.used.Load() >= maxBytes {
			return errRepairByteLimit
		}
		// Private object IDs must never be requested from a public source.
		private := r.Private || (g.policy != nil && g.policy().Reads == "members")
		if private && !g.privatePeer(raw) {
			lastErr = errors.New("blocked: private Git source is not a configured peer")
			continue
		}
		source, ip, err := g.resolvedSource(ctx, raw)
		if err != nil {
			lastErr = err
			continue
		}
		var proxy *privateProxy
		if peerBase := g.privatePeerBase(source); peerBase != "" {
			if err := probePrivatePeer(ctx, peerBase, g.allowPrivate); err != nil {
				continue
			}
			g.mu.RLock()
			signer := g.httpAuth
			g.mu.RUnlock()
			proxy, err = newPrivateProxy(ctx, source, signer, g.allowPrivate)
			if err != nil {
				continue
			}
			defer proxy.Close(context.Background())
		}
		fetchSource := source
		if proxy != nil {
			fetchSource = proxy.URL()
		}
		var budgetProxy *repairProxy
		if budget != nil {
			targetIP := ip
			if proxy != nil {
				if parsed, parseErr := url.Parse(proxy.URL()); parseErr == nil {
					targetIP = parsed.Hostname()
				}
			}
			parsedSource, parseErr := url.Parse(fetchSource)
			if parseErr != nil {
				lastErr = parseErr
				continue
			}
			targetHost := parsedSource.Hostname()
			targetPort := parsedSource.Port()
			if targetPort == "" {
				targetPort = "443"
				if parsedSource.Scheme == "http" {
					targetPort = "80"
				}
			}
			budgetProxy, err = newRepairProxy(ctx, targetIP, targetHost, targetPort, budget, maxBytes)
			if err != nil {
				lastErr = err
				continue
			}
			defer budgetProxy.Close()
		}
		for ref, oid := range expected {
			if err := ctx.Err(); err != nil {
				return err
			}
			if budget != nil && budget.used.Load() >= maxBytes {
				return errRepairByteLimit
			}
			if _, needed := missing[ref]; budget != nil && !needed {
				continue
			}
			if oid == "" {
				delete(missing, ref)
				continue
			}
			port := uPort(source)
			resolve := []string{"-c", "http.curloptResolve=" + urlHost(source) + ":" + port + ":" + ip}
			if proxy != nil {
				fetchSource = proxy.URL()
				resolve = nil
			}
			if budgetProxy != nil {
				resolve = nil
			}
			fetchRef := ref
			if strings.HasPrefix(ref, "refs/nostr/") {
				// NIP-34 clone URLs may point to ordinary Git hosts. The
				// signed commit is the source; refs/nostr is our local name.
				if !isObjectID(oid) {
					return errors.New("invalid: PR commit ID")
				}
				fetchRef = oid
			}
			proxySetting := ""
			if budgetProxy != nil {
				proxySetting = budgetProxy.URL()
			}
			args := []string{"-c", "http.followRedirects=false", "-c", "credential.helper=", "-c", "http.proxy=" + proxySetting, "--git-dir", g.repoPath(r), "fetch", "--no-tags"}
			if depth > 0 {
				args = append(args, "--depth", strconv.Itoa(depth))
			}
			args = append(args, fetchSource, "+"+fetchRef+":"+"refs/tinyrelay/source/"+safeRef(ref))
			// Keep the resolve option before the subcommand; Git accepts config
			// options only before the command name.
			if len(resolve) > 0 {
				args = append([]string{"-c", "http.curloptResolve=" + urlHost(source) + ":" + port + ":" + ip}, args...)
			}
			cmd := exec.CommandContext(ctx, "git", args...)
			if depth > 0 && maxBytes > 0 {
				// Git receives pack data in a child process and does not expose a
				// portable receive-byte limit. Apply the shell's file-size limit
				// to the isolated fetch process so an oversized pack is stopped
				// while it is being written. The unit is 512 bytes in dash and
				// 1024 in bash, so this is a coarse guard; the proxy budget and
				// the cumulative size check below enforce the exact bound.
				limitBlocks := (maxBytes + 511) / 512
				limited := []string{"ulimit -f \"$1\"; shift; exec \"$@\"", "gitfetch", strconv.FormatInt(limitBlocks, 10), "git"}
				limited = append(limited, args...)
				cmd = exec.CommandContext(ctx, "sh", append([]string{"-c"}, limited...)...)
			}
			cmd.Env = append(os.Environ(), "HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "http_proxy=", "https_proxy=", "all_proxy=", "NO_PROXY=", "no_proxy=", "GIT_CONFIG_NOSYSTEM=1")
			if budget != nil {
				// A URL-specific proxy or rewrite in the user's Git config must
				// not bypass the bounded transport selected for this repair.
				cmd.Env = append(cmd.Env, "GIT_CONFIG_GLOBAL="+os.DevNull)
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				lastErr = fmt.Errorf("fetch %s: %w: %s", fetchSource, err, strings.TrimSpace(string(out)))
				continue
			}
			if maxBytes > 0 {
				size, err := gitDirSize(g.repoPath(r))
				if err != nil {
					lastErr = fmt.Errorf("inspect isolated Git repository: %w", err)
					continue
				}
				if size > maxBytes {
					return fmt.Errorf("Git source exceeded isolated repair budget: %d bytes", size)
				}
			}
			if !g.objectsPresent(ctx, r, []string{oid}) {
				continue
			}
			delete(missing, ref)
		}
	}
	if len(missing) != 0 {
		if lastErr != nil {
			return fmt.Errorf("Git sources did not provide every expected object: %w", lastErr)
		}
		return errors.New("Git sources did not provide every expected object")
	}
	return g.installFetchedRefs(r, expected)
}

func (g *GitRelay) installFetchedRefs(r Repository, expected map[string]string) error {
	native, err := g.nativeRefs(r)
	if err != nil {
		return err
	}
	for ref, oid := range expected {
		if oid != "" && native[ref] != oid {
			if err := g.updateRef(r, ref, oid); err != nil {
				return err
			}
		}
	}
	return nil
}

func urlHost(raw string) string {
	u, _ := url.Parse(raw)
	h := u.Hostname()
	if strings.Contains(h, ":") {
		return "[" + h + "]"
	}
	return h
}
func uPort(raw string) string {
	u, _ := url.Parse(raw)
	if p := u.Port(); p != "" {
		return p
	}
	return "443"
}

func safeRef(ref string) string {
	return strings.NewReplacer("/", "_", "\\", "_").Replace(strings.TrimPrefix(ref, "refs/"))
}

func gitDirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func (g *GitRelay) updateRef(r Repository, ref, oid string) error {
	cmd := exec.Command("git", "--git-dir", g.repoPath(r), "update-ref", ref, oid)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("update ref %s: %w: %s", ref, err, strings.TrimSpace(string(out)))
	}
	return nil
}
func isObjectID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (g *GitRelay) repoPath(r Repository) string {
	if r.Alternative {
		return filepath.Join(g.root, "prs", r.Owner, r.Identifier+".git")
	}
	return filepath.Join(g.root, r.Owner, r.Identifier+".git")
}
func (g *GitRelay) statePath(r Repository) string {
	return filepath.Join(g.repoPath(r), "tinyrelay.refs")
}
func (g *GitRelay) pendingStatePath(r Repository) string {
	return filepath.Join(g.repoPath(r), "tinyrelay.pending")
}

func (g *GitRelay) pendingHeadPath(r Repository) string {
	return filepath.Join(g.repoPath(r), "tinyrelay.pending.head")
}

func (g *GitRelay) stateObjectsPresent(ctx context.Context, r Repository) bool {
	oids := make([]string, 0, len(r.Refs))
	for _, oid := range r.Refs {
		if oid != "" {
			oids = append(oids, oid)
		}
	}
	return g.objectsPresent(ctx, r, oids)
}

// objectsPresent checks all object IDs through one long-lived cat-file
// process. Repository state can contain hundreds of refs; starting one Git
// process per ref makes promotion latency grow linearly with process startup.
func (g *GitRelay) objectsPresent(ctx context.Context, r Repository, oids []string) bool {
	if len(oids) == 0 {
		return true
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir", g.repoPath(r), "cat-file", "--batch-check")
	var out bytes.Buffer
	cmd.Stdin = strings.NewReader(strings.Join(oids, "\n") + "\n")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return false
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != len(oids) {
		return false
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] == "missing" {
			return false
		}
	}
	return true
}
func (g *GitRelay) ensureRepo(r Repository) error {
	p := g.repoPath(r)
	if _, err := os.Stat(filepath.Join(p, "HEAD")); err == nil {
		return g.configureRepo(r)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	cmd := exec.Command("git", "init", "--bare", p)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return g.configureRepo(r)
}

func (g *GitRelay) configureRepo(r Repository) error {
	g.repoConfigMu.Lock()
	defer g.repoConfigMu.Unlock()
	if _, ok := g.configured[g.repoPath(r)]; ok {
		return nil
	}
	for _, pair := range [][2]string{{"http.receivepack", "true"}, {"http.uploadpack", "true"}} {
		cmd := exec.Command("git", "--git-dir", g.repoPath(r), "config", pair[0], pair[1])
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git config %s: %w: %s", pair[0], err, strings.TrimSpace(string(out)))
		}
	}
	if err := g.installHook(r); err != nil {
		return err
	}
	if g.configured == nil {
		g.configured = make(map[string]struct{})
	}
	g.configured[g.repoPath(r)] = struct{}{}
	return nil
}

func (g *GitRelay) installHook(r Repository) error {
	hook := filepath.Join(g.repoPath(r), "hooks", "pre-receive")
	state, pending := g.statePath(r), g.pendingStatePath(r)
	// Load signed maps once per push, not one process per ref. The hook is
	// static; metadata changes only replace the signed ref files.
	script := "#!/bin/sh\nset -eu\nTINY_REFS=" + shellQuote(state) + " TINY_PENDING=" + shellQuote(pending) + ` awk '
BEGIN {
  while ((getline line < ENVIRON["TINY_REFS"]) > 0) { split(line, row, " "); expected[row[1]]=row[2] }
  while ((getline line < ENVIRON["TINY_PENDING"]) > 0) { split(line, row, " "); expected[row[1]]=row[2] }
}
NF != 3 || expected[$3] != $2 {
  print "tinyrelay: ref is not authorized by signed repository state" > "/dev/stderr"
  exit 1
}
'
`
	if err := atomicExecutable(hook, []byte(script)); err != nil {
		return err
	}
	post := filepath.Join(g.repoPath(r), "hooks", "post-receive")
	postScript := "#!/bin/sh\nset -eu\nif [ -f " + shellQuote(pending) + " ]; then\n  checked=$(awk '{print $2}' " + shellQuote(pending) + " | git cat-file --batch-check) || exit 1\n  if printf '%s\\n' \"$checked\" | awk 'NF != 3 || $2 == \"missing\" { bad=1 } END { exit bad }'; then mv -f " + shellQuote(pending) + " " + shellQuote(state) + "; fi\nfi\n"
	return atomicExecutable(post, []byte(postScript))
}

func atomicExecutable(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tinyrelay-hook-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0700); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func (g *GitRelay) writeState(r Repository) error {
	if err := g.ensureRepo(r); err != nil {
		return err
	}
	if err := g.projectNativeRefs(r, r.Refs); err != nil {
		return err
	}
	if err := g.writeRefsFile(g.statePath(r), r.Refs); err != nil {
		return err
	}
	if r.Head != "" {
		ref := strings.TrimPrefix(r.Head, "ref: ")
		if out, err := exec.Command("git", "--git-dir", g.repoPath(r), "symbolic-ref", "HEAD", ref).CombinedOutput(); err != nil {
			return fmt.Errorf("install Git HEAD: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func (g *GitRelay) projectNativeRefs(r Repository, next map[string]string) error {
	if len(next) > 0 && !g.objectsPresent(context.Background(), r, refObjectIDs(next)) {
		// Alternative PR metadata may arrive before its object. Keep the
		// signed file authoritative for the receive hook, but do not create a
		// native ref Git cannot resolve until the object is present.
		return nil
	}
	previous, err := g.nativeRefs(r)
	if err != nil {
		return err
	}
	refs := make(map[string]struct{}, len(previous)+len(next))
	for ref := range previous {
		refs[ref] = struct{}{}
	}
	for ref := range next {
		if !internalRef(ref) {
			refs[ref] = struct{}{}
		}
	}
	keys := make([]string, 0, len(refs))
	for ref := range refs {
		keys = append(keys, ref)
	}
	sort.Strings(keys)
	var tx strings.Builder
	tx.WriteString("start\n")
	for _, ref := range keys {
		if oid, ok := next[ref]; ok && !internalRef(ref) {
			if previousOID, exists := previous[ref]; exists && previousOID == oid {
				continue
			}
			tx.WriteString("update ")
			tx.WriteString(ref)
			tx.WriteByte(' ')
			tx.WriteString(oid)
			tx.WriteByte('\n')
			continue
		}
		if _, ok := previous[ref]; ok {
			tx.WriteString("delete ")
			tx.WriteString(ref)
			tx.WriteByte('\n')
		}
	}
	tx.WriteString("prepare\ncommit\n")
	cmd := exec.Command("git", "--git-dir", g.repoPath(r), "update-ref", "--stdin")
	cmd.Stdin = strings.NewReader(tx.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("project Git refs: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (g *GitRelay) nativeRefs(r Repository) (map[string]string, error) {
	cmd := exec.Command("git", "--git-dir", g.repoPath(r), "for-each-ref", "--format=%(refname) %(objectname)")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read native Git refs: %w", err)
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && !internalRef(fields[0]) {
			refs[fields[0]] = fields[1]
		}
	}
	return refs, nil
}

func refObjectIDs(refs map[string]string) []string {
	oids := make([]string, 0, len(refs))
	for _, oid := range refs {
		if oid != "" {
			oids = append(oids, oid)
		}
	}
	return oids
}

func internalRef(ref string) bool { return strings.HasPrefix(ref, "refs/tinyrelay/") }

func readRefsFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			refs[fields[0]] = fields[1]
		}
	}
	return refs, nil
}

func (g *GitRelay) writePendingState(r Repository) error {
	if err := g.ensureRepo(r); err != nil {
		return err
	}
	if err := g.writeRefsFile(g.pendingStatePath(r), r.Refs); err != nil {
		return err
	}
	if r.Head == "" {
		return nil
	}
	return os.WriteFile(g.pendingHeadPath(r), []byte(r.Head+"\n"), 0600)
}

func (g *GitRelay) writeRefsFile(path string, refs map[string]string) error {
	keys := make([]string, 0, len(refs))
	for ref := range refs {
		keys = append(keys, ref)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, ref := range keys {
		b.WriteString(ref)
		b.WriteByte(' ')
		b.WriteString(refs[ref])
		b.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0600); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (g *GitRelay) lookup(owner, id string) (Repository, error) {
	g.mu.RLock()
	r, ok := g.repos[key(owner, id)]
	g.mu.RUnlock()
	if ok {
		return r, nil
	}
	q, err := g.store.Query(context.Background(), event.Filter{Authors: []string{owner}, Kinds: []int{30617}, Tags: map[string][]string{"d": {id}}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
	if err != nil {
		return Repository{}, err
	}
	if len(q.Events) == 0 {
		return Repository{}, os.ErrNotExist
	}
	r, err = g.parseRepository(q.Events[0])
	if err != nil {
		return Repository{}, err
	}
	// The event store may acknowledge a state before the asynchronous Git
	// work intent has run. Load the latest signed state here so an immediate
	// receive-pack request can stage its authorization hook synchronously.
	states, stateErr := g.store.Query(context.Background(), event.Filter{Authors: []string{owner}, Kinds: []int{30618}, Tags: map[string][]string{"d": {id}}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
	if stateErr == nil && len(states.Events) > 0 {
		if state, parseErr := g.parseRepository(states.Events[0]); parseErr == nil {
			state.Clone, state.Relays, state.Maintainers = r.Clone, r.Relays, r.Maintainers
			r = state
		}
	}
	g.mu.Lock()
	g.repos[key(owner, id)] = r
	g.mu.Unlock()
	return r, nil
}

// ServeHTTP exposes Git smart HTTP at /npub/<identifier>.git and /prs/... .
// The subprocess boundary keeps pack parsing and object integrity in Git.
func (g *GitRelay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Git-Protocol, Authorization")
	if req.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	owner, id, suffix, alternative, ok := parsePathMode(req.URL.Path)
	if !ok {
		http.NotFound(w, req)
		return
	}
	r, err := g.lookup(owner, id)
	if alternative {
		g.mu.RLock()
		candidate, found := g.repos[key(owner, id)]
		g.mu.RUnlock()
		if found && candidate.Alternative {
			r, err = candidate, nil
		}
	}
	if err != nil {
		http.NotFound(w, req)
		return
	}
	if r.Private {
		// Private authorization may replace the body with a verified disk
		// spool. Close that replacement as well as the server-owned body.
		defer func() { _ = req.Body.Close() }()
		if g.authorizeHTTP == nil {
			http.Error(w, "private repository requires authorization", http.StatusForbidden)
			return
		}
		if err := g.authorizeHTTP(req.Context(), req, r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	}
	if err := g.ensureRepo(r); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if suffix == "" && (req.Method == http.MethodGet || req.Method == http.MethodHead) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><body><code>"+html.EscapeString(r.Owner+"/"+r.Identifier)+"</code></body></html>\n")
		return
	}
	if err := g.cgi(req, w, r, suffix); err != nil {
		var streamed *cgiStreamError
		if errors.As(err, &streamed) {
			slog.ErrorContext(req.Context(), "Git response stream failed", "error", err)
			// Close an incomplete pack, rather than mark it complete or append
			// an HTTP error page to Git's binary protocol.
			panic(http.ErrAbortHandler)
		}
		http.Error(w, err.Error(), 500)
		return
	}
	if req.Method == http.MethodPost {
		// The CGI response has already been committed. Promotion callbacks are
		// retried by the next scheduler pass; never corrupt a successful Git
		// receive-pack response with a late HTTP error.
		_ = g.PromotePending(req.Context(), r)
	}
}

func parsePath(path string) (owner, id, suffix string, ok bool) {
	owner, id, suffix, _, ok = parsePathMode(path)
	return
}

func parsePathMode(path string) (owner, id, suffix string, alternative, ok bool) {
	var err error
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		return
	}
	if parts[0] == "prs" {
		if len(parts) < 3 {
			return
		}
		parts = parts[1:]
		alternative = true
	}
	if len(parts) > 0 && parts[0] == "npub" {
		parts = parts[1:]
	}
	if len(parts) < 2 {
		return
	}
	if !strings.HasPrefix(parts[0], "npub1") || !strings.HasSuffix(parts[1], ".git") {
		return
	}
	owner, err = decodeNPub(parts[0])
	if err != nil {
		return
	}
	id, err = url.PathUnescape(strings.TrimSuffix(parts[1], ".git"))
	if err != nil || !validIdentifier(id) {
		return
	}
	if len(parts) > 2 {
		suffix = "/" + strings.Join(parts[2:], "/")
	}
	ok = true
	return
}

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func decodeNPub(s string) (string, error) {
	s = strings.ToLower(s)
	if !strings.HasPrefix(s, "npub1") {
		return "", errors.New("not an npub")
	}
	// Bech32 uses the first separator; the payload may itself contain the
	// character "1".
	pos := strings.IndexByte(s, '1')
	if pos < len("npub") || len(s)-pos < 7 {
		return "", errors.New("invalid npub")
	}
	data := make([]byte, 0, len(s)-pos-1)
	for _, c := range s[pos+1:] {
		i := strings.IndexRune(bech32Charset, c)
		if i < 0 {
			return "", errors.New("invalid npub character")
		}
		data = append(data, byte(i))
	}
	if bech32Polymod(append(hrpExpand(s[:pos]), data...)) != 1 {
		return "", errors.New("invalid npub checksum")
	}
	payload, err := convertBits(data[:len(data)-6], 5, 8, false)
	if err != nil || len(payload) != 32 {
		return "", errors.New("invalid npub payload")
	}
	return fmt.Sprintf("%x", payload), nil
}

func hrpExpand(s string) []byte {
	out := make([]byte, 0, len(s)*2+1)
	for _, c := range s {
		out = append(out, byte(c>>5))
	}
	out = append(out, 0)
	for _, c := range s {
		out = append(out, byte(c&31))
	}
	return out
}
func bech32Polymod(v []byte) uint64 {
	const g0 = 0x3b6a57b2
	const g1 = 0x26508e6d
	const g2 = 0x1ea119fa
	const g3 = 0x3d4233dd
	const g4 = 0x2a1462b3
	chk := uint64(1)
	for _, x := range v {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 | uint64(x)
		if top&1 != 0 {
			chk ^= g0
		}
		if top&2 != 0 {
			chk ^= g1
		}
		if top&4 != 0 {
			chk ^= g2
		}
		if top&8 != 0 {
			chk ^= g3
		}
		if top&16 != 0 {
			chk ^= g4
		}
	}
	return chk
}
func convertBits(data []byte, from, to uint, pad bool) ([]byte, error) {
	acc := uint(0)
	bits := uint(0)
	maxv := uint((1 << to) - 1)
	maxacc := uint((1 << (from + to - 1)) - 1)
	out := make([]byte, 0)
	for _, v := range data {
		if uint(v)>>from != 0 {
			return nil, errors.New("invalid bech32 data")
		}
		acc = (acc<<from | uint(v)) & maxacc
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte((acc>>bits)&maxv))
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte((acc<<(to-bits))&maxv))
		}
	} else if bits >= from || byte((acc<<(to-bits))&maxv) != 0 {
		return nil, errors.New("invalid bech32 padding")
	}
	return out, nil
}

func (g *GitRelay) cgi(req *http.Request, w http.ResponseWriter, r Repository, suffix string) error {
	prefix := ""
	if r.Alternative {
		prefix = "/prs"
	}
	pathInfo := prefix + "/" + r.Owner + "/" + r.Identifier + ".git" + suffix
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	// Browser clients such as gitworkshop.dev fetch one commit at a time with
	// a filter, and refuse servers that do not advertise filter and
	// sha1-in-want. The -c settings reach upload-pack through the environment.
	cmd := exec.CommandContext(ctx, "git", "-c", "uploadpack.allowFilter=true", "-c", "uploadpack.allowAnySHA1InWant=true", "http-backend")
	cmd.Dir = g.root
	cmd.WaitDelay = 5 * time.Second
	stdin, contentLength, cleanup, err := cgiInput(req)
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Stdin = stdin
	var stderr cgiDiagnostic
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer stdout.Close()
	query := req.URL.RawQuery
	env := os.Environ()
	env = append(env, "GIT_PROJECT_ROOT="+g.root, "GIT_HTTP_EXPORT_ALL=1", "PATH_INFO="+pathInfo, "PATH_TRANSLATED="+filepath.Join(g.root, strings.TrimPrefix(pathInfo, "/")), "REQUEST_METHOD="+req.Method, "QUERY_STRING="+query, "REMOTE_ADDR="+req.RemoteAddr)
	if value := req.Header.Get("Content-Type"); value != "" {
		env = append(env, "CONTENT_TYPE="+value)
	}
	if contentLength != "" {
		env = append(env, "CONTENT_LENGTH="+contentLength)
	}
	if value := req.Header.Get("Git-Protocol"); value != "" {
		env = append(env, "HTTP_GIT_PROTOCOL="+value)
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start git http-backend: %w", err)
	}
	controller := http.NewResponseController(w)
	_ = controller.EnableFullDuplex()
	reader := bufio.NewReader(stdout)
	started, streamErr := streamCGI(w, controller, reader)
	if streamErr != nil {
		cancel()
		_ = stdout.Close()
		_ = req.Body.Close()
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		streamErr = errors.Join(streamErr, fmt.Errorf("git http-backend: %w: %s", waitErr, strings.TrimSpace(stderr.String())))
	}
	if streamErr != nil && started {
		return &cgiStreamError{streamErr}
	}
	return streamErr
}

// Compressed smart-HTTP negotiation needs a decoded CONTENT_LENGTH for CGI.
// Spool this uncommon request form to disk so memory stays independent of size.
func cgiInput(req *http.Request) (io.Reader, string, func(), error) {
	length := req.Header.Get("Content-Length")
	if !strings.EqualFold(req.Header.Get("Content-Encoding"), "gzip") {
		return req.Body, length, func() {}, nil
	}
	zr, err := gzip.NewReader(req.Body)
	if err != nil {
		return nil, "", nil, fmt.Errorf("decode compressed Git request: %w", err)
	}
	defer zr.Close()
	file, err := os.CreateTemp("", "tiny-git-request-*")
	if err != nil {
		return nil, "", nil, err
	}
	cleanup := func() { _ = file.Close(); _ = os.Remove(file.Name()) }
	n, err := io.Copy(file, zr)
	if err == nil {
		_, err = file.Seek(0, io.SeekStart)
	}
	if err != nil {
		cleanup()
		return nil, "", nil, fmt.Errorf("spool compressed Git request: %w", err)
	}
	return file, strconv.FormatInt(n, 10), cleanup, nil
}

type cgiStreamError struct{ error }

// Only diagnostic text is bounded; pack streams have no product size limit.
type cgiDiagnostic struct{ bytes.Buffer }

func (b *cgiDiagnostic) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 64*1024 - b.Len(); remaining > 0 {
		_, _ = b.Buffer.Write(p[:min(remaining, n)])
	}
	return n, nil
}

func streamCGI(w http.ResponseWriter, controller *http.ResponseController, reader *bufio.Reader) (bool, error) {
	status := http.StatusOK
	responseHeaders := make(http.Header)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return false, fmt.Errorf("read Git response headers: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return false, errors.New("malformed Git CGI response header")
		}
		if strings.EqualFold(k, "Status") {
			if _, err := fmt.Sscanf(v, "%d", &status); err != nil || status < 200 || status > 599 {
				return false, errors.New("invalid Git CGI response status")
			}
		} else {
			responseHeaders.Add(k, strings.TrimSpace(v))
		}
	}
	// Hold the response until git produces its first body byte. HTTP/1.1
	// proxies without full duplex discard the unread request body once a
	// response starts, which would truncate a pack that is still uploading.
	// git-http-backend writes its headers before reading the request, but
	// writes body bytes only after it has consumed the pack.
	if _, err := reader.Peek(1); err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read Git response body: %w", err)
	}
	for k, values := range responseHeaders {
		for _, value := range values {
			w.Header().Add(k, value)
		}
	}
	w.WriteHeader(status)
	if err := controller.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return true, err
	}
	_, err := io.Copy(cgiFlushWriter{w, controller}, reader)
	return true, err
}

type cgiFlushWriter struct {
	writer     io.Writer
	controller *http.ResponseController
}

func (w cgiFlushWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err == nil {
		err = w.controller.Flush()
		if errors.Is(err, http.ErrNotSupported) {
			err = nil
		}
	}
	return n, err
}

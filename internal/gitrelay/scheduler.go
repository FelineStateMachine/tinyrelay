package gitrelay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

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
		if errors.Is(repoErr, ErrIncomplete) {
			// Progress is saved; continue soon without recording a failure.
			if previous, ok := previousProgress.Repos[repoKey]; ok {
				status.SucceededAt = previous.SucceededAt
			}
			status.NextAt = progress.At + graspRetryDelay
		} else if repoErr != nil {
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

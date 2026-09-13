package tinygit

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	event "github.com/FelineStateMachine/tinyrelay/protocol/nostr"
)

// ServerConfig configures the standalone public Git host. Use a dedicated
// DataDir, not a running tinyrelay tenant's database or Git directory.
type ServerConfig struct {
	DataDir   string
	Owner     string
	PublicURL string
}

// Server owns a Git engine and durable SQLite metadata, without a relay daemon,
// WebSocket service, UI or replication workers. POST /events accepts signed
// repository metadata. Other paths are handled by Git smart HTTP.
type Server struct {
	git       *GitRelay
	store     Store
	publishMu sync.Mutex
}

var errHostAdmission = errors.New("blocked: standalone host accepts only its owner's public repositories and their maintainers")

// OpenServer opens the standalone host. Stop all HTTP requests before Close.
func OpenServer(ctx context.Context, cfg ServerConfig) (*Server, error) {
	owner, err := hex.DecodeString(cfg.Owner)
	if err != nil || len(owner) != 32 || cfg.Owner != strings.ToLower(cfg.Owner) {
		return nil, errors.New("tinygit: owner must be a 64-character lowercase hex public key")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("tinygit: data directory is required")
	}
	if cfg.PublicURL != "" {
		u, err := url.Parse(cfg.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("tinygit: public URL must be an HTTP(S) URL without credentials, query or fragment")
		}
	}
	cfg.DataDir, err = filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	store, err := OpenStore(ctx, filepath.Join(cfg.DataDir, "events.db"))
	if err != nil {
		return nil, err
	}
	g, err := New(Config{
		Store: store, Root: filepath.Join(cfg.DataDir, "git"), PublicURL: cfg.PublicURL,
		Authorize: func(_ context.Context, e Event, repo Repository) error {
			if repo.Owner != cfg.Owner || repo.Private || (e.PubKey != repo.Owner && !contains(repo.Maintainers, e.PubKey)) {
				return errHostAdmission
			}
			return nil
		},
	})
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	return &Server{git: g, store: store}, nil
}

// Close releases metadata storage. The caller must first stop serving requests.
func (s *Server) Close() error { return s.store.Close() }

// Publish validates and persists a signed announcement (30617) or ref state
// (30618). Repeated events retry the Git transition after a partial failure.
func (s *Server) Publish(ctx context.Context, e Event) error {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	repo, err := s.git.Validate(ctx, e)
	if err != nil {
		return err
	}
	if err := s.store.Save(ctx, e, time.Now().Unix()); err != nil && !errors.Is(err, ErrDuplicate) {
		return err
	}
	return s.git.CommitAfterStore(ctx, e, repo)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, "ok\n")
		}
		return
	}
	if r.URL.Path != "/events" {
		s.git.ServeHTTP(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "event exceeds 1 MiB", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "cannot read event", http.StatusBadRequest)
		}
		return
	}
	e, err := event.Parse(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Publish(r.Context(), e); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errHostAdmission) {
			status = http.StatusForbidden
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		ID      string `json:"id"`
		Pending bool   `json:"pending"`
	}{e.ID, s.git.IsPending(e.ID)})
}

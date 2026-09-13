package tinyrelay

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/FelineStateMachine/tinyrelay/internal/relayapp"
)

// ServerConfig configures a standalone relay. Owner is an optional operator
// public key for NIP-11 metadata; no signing key is required. AuthRequired
// requires NIP-42 authentication for reads and each publishing author.
type ServerConfig struct {
	DataDir         string
	PublicURL       string
	Name            string
	Description     string
	Contact         string
	Owner           string
	AuthRequired    bool
	MaxMessageBytes int64
	MaxPendingBytes int
}

// Server owns a standalone relay and its durable event store. It serves Nostr
// WebSockets, NIP-11 metadata, /healthz and /readyz without application services.
type Server struct {
	relay     *Relay
	backend   *relayapp.Backend
	relayPath string
	mu        sync.Mutex
	closed    bool
	requests  sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

var _ http.Handler = (*Server)(nil)

// OpenServer opens a database in DataDir without starting an HTTP listener.
// PublicURL must identify the client-facing relay, including any proxy prefix.
// HTTP(S) URLs are accepted and converted to WS(S) for authentication. Zero
// limits select defaults of 1 MiB per message and 4 MiB queued per connection.
func OpenServer(ctx context.Context, cfg ServerConfig) (*Server, error) {
	cfg, relayPath, err := normalizeServerConfig(cfg)
	if err != nil {
		return nil, err
	}
	backend, err := relayapp.Open(ctx, relayapp.Config(cfg))
	if err != nil {
		return nil, err
	}
	runtime, err := New(backend, Config{
		RelayURL: cfg.PublicURL, MaxMessageBytes: cfg.MaxMessageBytes,
		MaxPendingBytes: cfg.MaxPendingBytes, OriginPatterns: []string{"*"},
	})
	if err != nil {
		return nil, errors.Join(err, backend.Close())
	}
	return &Server{relay: runtime, backend: backend, relayPath: relayPath}, nil
}

func normalizeServerConfig(cfg ServerConfig) (ServerConfig, string, error) {
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return cfg, "", errors.New("tinyrelay: public URL must identify a relay without credentials, query or fragment")
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return cfg, "", errors.New("tinyrelay: public URL must use http, https, ws or wss")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	cfg.PublicURL = u.String()
	if cfg.Owner != "" {
		key, err := hex.DecodeString(cfg.Owner)
		if err != nil || len(key) != 32 || cfg.Owner != strings.ToLower(cfg.Owner) {
			return cfg, "", errors.New("tinyrelay: owner must be a lowercase 64-character hex public key")
		}
	}
	if cfg.MaxMessageBytes < 0 || cfg.MaxPendingBytes < 0 {
		return cfg, "", errors.New("tinyrelay: byte limits cannot be negative")
	}
	if cfg.MaxMessageBytes == 0 {
		cfg.MaxMessageBytes = 1 << 20
	}
	if cfg.MaxPendingBytes == 0 {
		cfg.MaxPendingBytes = 4 << 20
	}
	if cfg.Name == "" {
		cfg.Name = "tinyrelay"
	}
	return cfg, u.Path, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		http.Error(w, "relay is closed", http.StatusServiceUnavailable)
		return
	}
	s.requests.Add(1)
	s.mu.Unlock()
	defer s.requests.Done()
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	if r.URL.Path != "/" && r.URL.Path != s.relayPath && r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		s.serveHealth(w, r)
		return
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		s.relay.ServeHTTP(w, r)
		return
	}
	s.serveInformation(w, r)
}

func (s *Server) serveHealth(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/readyz" {
		if err := s.backend.Ready(r.Context()); err != nil {
			http.Error(w, "relay is not ready", http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte("ok\n"))
	}
}

func (s *Server) serveInformation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Vary", "Accept")
	if strings.Contains(r.Header.Get("Accept"), "application/nostr+json") {
		w.Header().Set("Content-Type", "application/nostr+json")
		if r.Method != http.MethodHead {
			// Information contains only JSON-safe metadata. A response write
			// failure means the requesting client has disconnected.
			_ = json.NewEncoder(w).Encode(s.backend.Information())
		}
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte("tinyrelay Nostr relay\nConnect with a Nostr WebSocket client.\n"))
	}
}

// Close rejects new requests, joins active connections and releases storage.
// It is safe to call concurrently or more than once. The HTTP listener remains
// owned by the caller and should also be shut down.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.requests.Wait()
		s.closeErr = errors.Join(s.relay.Close(context.Background()), s.backend.Close())
	})
	return s.closeErr
}

package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type PeerMonitorConfig struct {
	Peers       []string
	Interval    time.Duration
	Timeout     time.Duration
	Publish     bool
	PublishFunc func(context.Context, []PeerStatus) error
	Client      *http.Client
	Observe     func(outcome string, duration time.Duration)
}

type PeerStatus struct {
	URL         string        `json:"url"`
	Up          bool          `json:"up"`
	Latency     time.Duration `json:"latency"`
	LastChecked time.Time     `json:"last_checked"`
	LastSuccess time.Time     `json:"last_success,omitempty"`
	Error       string        `json:"error,omitempty"`
}

type PeerMonitor struct {
	interval  time.Duration
	timeout   time.Duration
	publish   bool
	client    *http.Client
	observe   func(string, time.Duration)
	publishFn func(context.Context, []PeerStatus) error
	mu        sync.RWMutex
	peers     map[string]PeerStatus
}

// SetPublisher installs the operator-selected publication sink. It is kept
// separate from probing so a monitor can be constructed before tenants are
// opened during process startup.
func (m *PeerMonitor) SetPublisher(publish func(context.Context, []PeerStatus) error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.publishFn = publish
	m.mu.Unlock()
}

func NewPeerMonitor(cfg PeerMonitorConfig) (*PeerMonitor, error) {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	peers := make(map[string]PeerStatus, len(cfg.Peers))
	for _, raw := range cfg.Peers {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("peer monitor: invalid peer URL %q", raw)
		}
		canonical := strings.TrimRight(u.String(), "/")
		peers[canonical] = PeerStatus{URL: canonical}
	}
	return &PeerMonitor{interval: cfg.Interval, timeout: cfg.Timeout, publish: cfg.Publish, client: cfg.Client, observe: cfg.Observe, publishFn: cfg.PublishFunc, peers: peers}, nil
}

func (m *PeerMonitor) PublicationEnabled() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.publish && m.publishFn != nil
}

func (m *PeerMonitor) Peers() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]string, 0, len(m.peers))
	for peer := range m.peers {
		result = append(result, peer)
	}
	sort.Strings(result)
	return result
}

func (m *PeerMonitor) Snapshot() []PeerStatus {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]PeerStatus, 0, len(m.peers))
	for _, status := range m.peers {
		result = append(result, status)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].URL < result[j].URL })
	return result
}

func (m *PeerMonitor) Probe(ctx context.Context) error {
	if m == nil {
		return errors.New("peer monitor: nil monitor")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client := m.client
	if client == nil {
		client = &http.Client{}
	}
	var firstErr error
	for _, peer := range m.Peers() {
		started := time.Now()
		probeCtx, cancel := context.WithTimeout(ctx, m.timeout)
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, peer, nil)
		if err == nil {
			req.Header.Set("Accept", "application/nostr+json")
			response, requestErr := client.Do(req)
			if requestErr == nil {
				_, _ = io.CopyN(io.Discard, response.Body, 1<<20)
				_ = response.Body.Close()
				if response.StatusCode < 200 || response.StatusCode >= 400 {
					requestErr = fmt.Errorf("peer returned HTTP %d", response.StatusCode)
				}
			}
			err = requestErr
		}
		cancel()
		latency := time.Since(started)
		status := PeerStatus{URL: peer, Up: err == nil, Latency: latency, LastChecked: time.Now()}
		if err == nil {
			m.mu.RLock()
			status.LastSuccess = m.peers[peer].LastSuccess
			m.mu.RUnlock()
			status.LastSuccess = status.LastChecked
		} else {
			status.Error = err.Error()
			m.mu.RLock()
			status.LastSuccess = m.peers[peer].LastSuccess
			m.mu.RUnlock()
			if firstErr == nil {
				firstErr = err
			}
		}
		m.mu.Lock()
		m.peers[peer] = status
		m.mu.Unlock()
		if m.observe != nil {
			outcome := "ok"
			if err != nil {
				outcome = "error"
			}
			m.observe(outcome, latency)
		}
	}
	m.mu.RLock()
	publish := m.publishFn
	m.mu.RUnlock()
	if m.publish && publish != nil {
		if err := publish(ctx, m.Snapshot()); err != nil {
			return fmt.Errorf("peer monitor: publish status: %w", err)
		}
	}
	return firstErr
}

func (m *PeerMonitor) Run(ctx context.Context) error {
	if m == nil {
		return errors.New("peer monitor: nil monitor")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_ = m.Probe(ctx)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_ = m.Probe(ctx)
		}
	}
}

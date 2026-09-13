// Package relayapp provides the storage-backed base relay used by the
// standalone tinyrelay command. It deliberately contains no hosted
// application features; those remain in daemon.
package relayapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/syncprotocol"
)

// Config controls one standalone relay database and its public metadata.
type Config struct {
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

// Backend is the storage-backed relay implementation. It can be passed to
// relay.New and is safe to use until Close returns.
type Backend struct {
	store     *storage.Store
	lock      *os.File
	cfg       Config
	closeOnce sync.Once
	closeErr  error
}

// Open creates or opens a standalone relay database below Config.DataDir.
func Open(ctx context.Context, cfg Config) (*Backend, error) {
	if strings.TrimSpace(cfg.DataDir) == "" {
		return nil, errors.New("relayapp: data directory is required")
	}
	dataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("relayapp: resolve data directory: %w", err)
	}
	cfg.DataDir = dataDir
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("relayapp: create data directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(dataDir, ".tinyrelay.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("relayapp: open data lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(errors.New("relayapp: data directory is already in use"), lock.Close())
		}
		return nil, errors.Join(fmt.Errorf("relayapp: lock data directory: %w", err), lock.Close())
	}
	store, err := storage.Open(ctx, filepath.Join(dataDir, "events.db"))
	if err != nil {
		return nil, errors.Join(err, syscall.Flock(int(lock.Fd()), syscall.LOCK_UN), lock.Close())
	}
	return &Backend{store: store, lock: lock, cfg: cfg}, nil
}

// Ready verifies that the database is available.
func (b *Backend) Ready(ctx context.Context) error {
	if b == nil || b.store == nil {
		return errors.New("relayapp: backend is closed")
	}
	return b.store.DB().PingContext(ctx)
}

// Close releases the database.
func (b *Backend) Close() error {
	if b == nil || b.store == nil {
		return nil
	}
	b.closeOnce.Do(func() {
		storeErr := b.store.Close()
		unlockErr := syscall.Flock(int(b.lock.Fd()), syscall.LOCK_UN)
		closeLockErr := b.lock.Close()
		b.closeErr = errors.Join(storeErr, unlockErr, closeLockErr)
	})
	return b.closeErr
}

func (b *Backend) Publish(ctx context.Context, e event.Event, s relay.Session) (string, error) {
	if b.store == nil {
		return "", errors.New("relayapp: backend is closed")
	}
	now := time.Now().Unix()
	if b.cfg.AuthRequired && !contains(s.PubKeys, e.PubKey) {
		return "", errors.New("auth-required: publishing requires AUTH")
	}
	if hasTag(e, "-") && !contains(s.PubKeys, e.PubKey) {
		return "", errors.New("auth-required: this event may only be published by its author")
	}
	if e.Kind == event.KIND_AUTH {
		return "", errors.New("blocked: AUTH events use AUTH")
	}
	if err := event.Validate(e); err != nil {
		return "", fmt.Errorf("invalid: %w", err)
	}
	if e.CreatedAt > now+900 {
		return "", errors.New("invalid: event creation date is too far off from the current time")
	}
	if expiration := event.Expiration(e); expiration > 0 && expiration <= now {
		return "", errors.New("invalid: event has already expired")
	}
	_, err := b.store.Save(ctx, e, storage.SaveOptions{Now: now})
	if errors.Is(err, storage.ErrDuplicate) {
		return storage.ErrDuplicate.Error(), nil
	}
	return "", err
}

func (b *Backend) Query(ctx context.Context, filters []event.Filter, s relay.Session) ([]event.Event, error) {
	if b.store == nil {
		return nil, errors.New("relayapp: backend is closed")
	}
	if b.cfg.AuthRequired && len(s.PubKeys) == 0 {
		return nil, errors.New("auth-required: this relay requires AUTH")
	}
	seen := make(map[string]struct{})
	var result []event.Event
	for _, f := range filters {
		row, err := b.store.Query(ctx, f, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true, IncludeCallbackRegistrations: true}})
		if err != nil {
			return nil, err
		}
		for _, e := range row.Events {
			if _, ok := seen[e.ID]; ok {
				continue
			}
			seen[e.ID] = struct{}{}
			result = append(result, e)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt == result[j].CreatedAt {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt > result[j].CreatedAt
	})
	return result, nil
}

func (b *Backend) Count(ctx context.Context, filters []event.Filter, s relay.Session) (any, error) {
	copyFilters := append([]event.Filter(nil), filters...)
	for i := range copyFilters {
		copyFilters[i].Limit = nil
	}
	events, err := b.Query(ctx, copyFilters, s)
	if err != nil {
		return nil, err
	}
	return map[string]any{"count": len(events)}, nil
}

func (b *Backend) Sync(ctx context.Context, f event.Filter, s relay.Session) ([]syncprotocol.Item, error) {
	f.Limit = nil
	events, err := b.Query(ctx, []event.Filter{f}, s)
	if err != nil {
		return nil, err
	}
	items := make([]syncprotocol.Item, len(events))
	for i, e := range events {
		items[i] = syncprotocol.Item{ID: e.ID, Timestamp: e.CreatedAt}
	}
	return items, nil
}

// CanRead applies current policy to a live event.
func (b *Backend) CanRead(e event.Event, s relay.Session) bool {
	return (!b.cfg.AuthRequired || len(s.PubKeys) > 0) && visible(e)
}

// CanReadFilter applies current policy with subscription filter context.
func (b *Backend) CanReadFilter(e event.Event, s relay.Session, f *event.Filter) bool {
	return b.CanRead(e, s)
}

func visible(e event.Event) bool {
	expires := event.Expiration(e)
	return expires == 0 || expires > time.Now().Unix()
}

func hasTag(e event.Event, name string) bool {
	for _, tag := range e.Tags {
		if len(tag) > 0 && tag[0] == name {
			return true
		}
	}
	return false
}

// Information returns conservative NIP-11 metadata for this relay.
func (b *Backend) Information() map[string]any {
	info := map[string]any{"name": b.cfg.Name, "description": b.cfg.Description, "supported_nips": []int{1, 11, 42, 45, 77}, "software": "https://github.com/FelineStateMachine/tinyrelay"}
	limitation := map[string]any{"auth_required": b.cfg.AuthRequired}
	if b.cfg.MaxMessageBytes > 0 {
		limitation["max_message_length"] = b.cfg.MaxMessageBytes
	}
	info["limitation"] = limitation
	if b.cfg.Contact != "" {
		info["contact"] = b.cfg.Contact
	}
	if b.cfg.Owner != "" {
		info["pubkey"] = b.cfg.Owner
	}
	return info
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

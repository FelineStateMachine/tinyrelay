package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/auth"
	"github.com/FelineStateMachine/tinyrelay/internal/blob"
	"github.com/FelineStateMachine/tinyrelay/internal/catalog"
	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/configport"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gates"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/records"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/syncprotocol"
	"github.com/FelineStateMachine/tinyrelay/internal/webui"
)

type tenantConfig struct {
	app       *App
	meta      catalog.Tenant
	store     *storage.Store
	policy    policy.Policy
	publicURL string
}

type Tenant struct {
	app         *App
	meta        catalog.Tenant
	store       *storage.Store
	community   *community.Service
	gate        *gates.Gate
	router      *relay.Router
	auth        *auth.Validator
	blobs       *blob.Service
	sites       *sites.Service
	records     *records.Service
	config      *configport.ConfigStore
	replication *replication.Service
	git         *gitrelay.GitRelay
	ui          *webui.App
	workCtx     context.Context
	workCancel  context.CancelFunc
	workWG      sync.WaitGroup
	publicURL   string
	mu          sync.RWMutex
	policy      policy.Policy
}

func newTenant(ctx context.Context, cfg tenantConfig) (*Tenant, error) {
	t := &Tenant{app: cfg.app, meta: cfg.meta, store: cfg.store, policy: cfg.policy, publicURL: cfg.publicURL, auth: auth.NewValidator(time.Now)}
	var err error
	t.community, err = community.New(ctx, t.store, t.policy.Owner)
	if err != nil {
		return nil, err
	}
	t.gate, err = gates.New(gates.Config{Store: t.store, Community: t.community, Policy: t.Policy, Slug: t.meta.Name})
	if err != nil {
		return nil, err
	}
	t.router = relay.New(t, relay.Config{RelayURL: t.RelayURL(), MaxMessageBytes: t.app.cfg.MaxMessageBytes, MaxPendingBytes: t.app.cfg.MaxPendingBytes, OriginPatterns: []string{"*"}})
	if err := t.initServices(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Tenant) Policy() policy.Policy {
	t.mu.RLock()
	defer t.mu.RUnlock()
	// Policy patches produce a fresh value; callers only inspect this snapshot.
	return t.policy
}
func (t *Tenant) RelayURL() string { return strings.Replace(t.publicURL, "http", "ws", 1) }
func (t *Tenant) Close(ctx context.Context) error {
	return errors.Join(t.closeServices(ctx), t.router.Close(ctx), t.store.Close())
}

func (t *Tenant) Publish(ctx context.Context, e event.Event, s relay.Session) (string, error) {
	ctx, finish := t.app.telemetry.Start(ctx, "publish")
	outcome := "error"
	defer func() { finish(outcome) }()
	now := time.Now().Unix()
	if err := t.gate.Write(ctx, e, s, now); err != nil {
		return "", err
	}
	if e.Kind == event.KIND_VANISH {
		if err := t.vanish(ctx, e); err != nil {
			return "", err
		}
		outcome = "ok"
		return "", nil
	}
	opts := storage.SaveOptions{Now: now, SearchMode: t.Policy().Features.Search}
	if t.sites != nil {
		if err := sites.ValidateManifest(e); err != nil {
			return "", err
		}
		opts = t.sites.SaveOptions(e, now)
		opts.SearchMode = t.Policy().Features.Search
		opts.Intents = append(opts.Intents, t.replication.Prepare(e, replication.OriginClient)...)
	}
	_, err := t.store.Save(ctx, e, opts)
	if errors.Is(err, storage.ErrDuplicate) {
		outcome = "duplicate"
		return storage.ErrDuplicate.Error(), nil
	}
	if err != nil {
		return "", err
	}
	outcome = "ok"
	return "", nil
}

func (t *Tenant) vanish(ctx context.Context, e event.Event) error {
	for _, tag := range e.Tags {
		if len(tag) > 1 && tag[0] == "relay" && (tag[1] == "ALL_RELAYS" || strings.TrimSuffix(tag[1], "/") == strings.TrimSuffix(t.RelayURL(), "/")) {
			return t.store.Vanish(ctx, e.PubKey, e.CreatedAt)
		}
	}
	return errors.New("invalid: vanish request does not name this relay")
}

func (t *Tenant) Query(ctx context.Context, filters []event.Filter, s relay.Session) ([]event.Event, error) {
	if _, err := t.gate.Read(ctx, filters, s); err != nil {
		return nil, err
	}
	result := []event.Event{}
	seen := make(map[string]bool)
	for _, f := range filters {
		rows, err := t.store.Query(ctx, f, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{PubKeys: s.PubKeys}})
		if err != nil {
			return nil, err
		}
		for _, e := range rows.Events {
			if !seen[e.ID] && t.gate.CanSee(ctx, e, s, &f) {
				seen[e.ID] = true
				result = append(result, e)
			}
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

func (t *Tenant) Count(ctx context.Context, filters []event.Filter, s relay.Session) (any, error) {
	if !t.Policy().Features.Count {
		return nil, errors.New("unsupported: COUNT is switched off on this relay")
	}
	// Count the same authorized union as REQ, with NIP-45 ignoring limit.
	unlimited := make([]event.Filter, len(filters))
	copy(unlimited, filters)
	for i := range unlimited {
		unlimited[i].Limit = nil
	}
	events, err := t.Query(ctx, unlimited, s)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"count": len(events)}
	if len(filters) == 1 {
		if offset, ok := syncprotocol.HLLFilterOffset(filters[0]); ok {
			hll := syncprotocol.NewHLL(offset)
			for _, e := range events {
				if err := hll.Add(e.PubKey); err != nil {
					return nil, err
				}
			}
			result["hll"] = hll.Hex()
		}
	}
	return result, nil
}

func (t *Tenant) Sync(ctx context.Context, f event.Filter, s relay.Session) ([]syncprotocol.Item, error) {
	if !t.Policy().Features.Sync {
		return nil, errors.New("unsupported: sync is switched off on this relay")
	}
	f.Limit = nil
	events, err := t.Query(ctx, []event.Filter{f}, s)
	if err != nil {
		return nil, err
	}
	items := make([]syncprotocol.Item, len(events))
	for i, e := range events {
		items[i] = syncprotocol.Item{ID: e.ID, Timestamp: e.CreatedAt}
	}
	return items, nil
}

func (t *Tenant) CanRead(e event.Event, s relay.Session) bool {
	return t.gate.CanSee(context.Background(), e, s, nil)
}

func (t *Tenant) CanReadFilter(e event.Event, s relay.Session, f *event.Filter) bool {
	return t.gate.CanSee(context.Background(), e, s, f)
}

func (t *Tenant) QueryHints(ctx context.Context, filters []event.Filter, s relay.Session) ([]event.Event, []string, error) {
	authHint, err := t.gate.Read(ctx, filters, s)
	if err != nil {
		return nil, nil, err
	}
	events, err := t.Query(ctx, filters, s)
	if err != nil {
		return nil, nil, err
	}
	hints := []string{"finish"}
	if authHint {
		hints = append(hints, "auth")
	}
	for _, f := range filters {
		if f.Limit == nil {
			continue
		}
		rows, err := t.store.Query(ctx, f, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{PubKeys: s.PubKeys}})
		if err != nil {
			return nil, nil, err
		}
		if rows.More {
			hints[0] = "more"
			break
		}
	}
	return events, hints, nil
}

func (t *Tenant) setPolicy(ctx context.Context, patch map[string]json.RawMessage) (policy.Policy, error) {
	t.mu.Lock()
	next, err := policy.Patch(t.policy, patch)
	if err == nil {
		err = t.store.PutSetting(ctx, "policy", next)
	}
	if err == nil {
		t.policy = next
	}
	t.mu.Unlock()
	if err == nil {
		t.router.CloseSubscriptions("blocked: relay policy changed; subscribe again")
	}
	return next, err
}

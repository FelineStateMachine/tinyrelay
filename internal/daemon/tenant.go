package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	"github.com/FelineStateMachine/tinyrelay/internal/mcp"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/records"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/syncprotocol"
	"github.com/FelineStateMachine/tinyrelay/internal/webpush"
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
	app           *App
	meta          catalog.Tenant
	store         *storage.Store
	community     *community.Service
	gate          *gates.Gate
	router        *relay.Router
	auth          *auth.Validator
	blobs         *blob.Service
	sites         *sites.Service
	records       *records.Service
	config        *configport.ConfigStore
	replication   *replication.Service
	git           *gitrelay.GitRelay
	ui            *webui.App
	mcp           *mcp.Server
	schedulerWake chan struct{}
	workCtx       context.Context
	workCancel    context.CancelFunc
	workWG        sync.WaitGroup
	workErrMu     sync.Mutex
	workErr       error
	publicURL     string
	mu            sync.RWMutex
	policyWrite   sync.Mutex
	policy        policy.Policy
	maintenance   maintenanceGate
	gitLegacy     *replication.LegacyCache
	pushMu        sync.Mutex
	pushVAPID     *webpush.Keys
	pushClient    *http.Client
	pushRecent    map[string]time.Time
	callbacks     *callbackService
	customViews   *customViewService
	followups     *eventFollowups
}

func newTenant(ctx context.Context, cfg tenantConfig) (*Tenant, error) {
	p := cfg.policy
	legacyPrivate, err := hasLegacyPrivateRepository(ctx, cfg.store)
	if err != nil {
		return nil, err
	}
	if legacyPrivate && (!p.Features.Grasp08 || p.Reads != "members") {
		// Keep the private boundary durable even if an announcement is later
		// removed while its repository or collaboration data remains.
		p.Features.Grasp = true
		p.Features.Grasp08 = true
		p.Reads = "members"
		if err := cfg.store.WithTx(ctx, func(tx *sql.Tx) error { return storage.PutSetting(ctx, tx, "policy", p) }); err != nil {
			return nil, fmt.Errorf("preserve legacy repository privacy: %w", err)
		}
	}
	t := &Tenant{app: cfg.app, meta: cfg.meta, store: cfg.store, policy: p, publicURL: cfg.publicURL, auth: auth.NewValidator(time.Now), schedulerWake: make(chan struct{}, 1), gitLegacy: &replication.LegacyCache{}}
	t.community, err = community.New(ctx, t.store, t.policy.Owner)
	if err != nil {
		return nil, err
	}
	if err := t.community.EnsureSlugRoom(ctx, t.meta.Name, community.RoomAccessFor(p.Reads)); err != nil {
		return nil, err
	}
	t.gate, err = gates.New(gates.Config{Store: t.store, Community: t.community, Policy: t.Policy, Slug: t.meta.Name})
	if err != nil {
		return nil, err
	}
	t.router = relay.New(t, InstrumentRelayConfig(relay.Config{RelayURL: t.RelayURL(), RequestRelayURL: func(r *http.Request) string { return strings.Replace(t.requestURL(r), "http", "ws", 1) }, OnAuthenticate: t.authenticated, MaxMessageBytes: t.app.cfg.MaxMessageBytes, MaxPendingBytes: t.app.cfg.MaxPendingBytes, OriginPatterns: []string{"*"}}, t.app.telemetry))
	if err := t.initServices(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Tenant) authenticated(ctx context.Context, s relay.Session) error {
	ctx, done, admissionErr := t.beginOperation(ctx)
	if admissionErr != nil {
		return admissionErr
	}
	defer done()
	for _, pubkey := range s.PubKeys {
		if privatePolicy(t.Policy()) {
			// Keep NIP-42 and NIP-98 at the same private-service boundary.
			// Membership is resolved for every AUTH, so revocation applies to
			// already connected clients when they authenticate again.
			if err := t.requirePrivateAccess(ctx, pubkey); err != nil {
				return err
			}
		}
		banned, err := t.community.IsBanned(ctx, pubkey)
		if err != nil {
			return err
		}
		if banned {
			return errors.New("blocked: this pubkey is banned")
		}
		if err := t.records.NotePresence(ctx, pubkey, time.Now().Unix()); err != nil {
			return err
		}
		if pubkey == t.Policy().Owner {
			if err := t.records.Heartbeat(ctx, pubkey, time.Now().Unix()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *Tenant) Policy() policy.Policy {
	t.mu.RLock()
	defer t.mu.RUnlock()
	// Policy patches produce a fresh value; callers only inspect this snapshot.
	return t.policy
}
func (t *Tenant) RelayURL() string { return strings.Replace(t.publicURL, "http", "ws", 1) }
func (t *Tenant) Close(ctx context.Context) error {
	t.maintenance.markClosing()
	servicesErr := t.closeServices(ctx)
	routerErr := t.router.Close(ctx)
	if err := t.maintenance.waitIdle(ctx); err != nil {
		return errors.Join(servicesErr, routerErr, err)
	}
	return errors.Join(servicesErr, routerErr, t.store.Close())
}

func (t *Tenant) Publish(ctx context.Context, e event.Event, s relay.Session) (string, error) {
	ctx, done, admissionErr := t.beginOperation(ctx)
	if admissionErr != nil {
		return "", admissionErr
	}
	defer done()
	ctx, finish := t.app.telemetry.Start(ctx, "publish")
	outcome := "error"
	defer func() { finish(outcome) }()
	now := time.Now().Unix()
	if err := t.gate.Write(ctx, e, s, now); err != nil {
		return "", err
	}
	if err := t.validateStoredEvent(ctx, e); err != nil {
		return "", err
	}
	if err := t.validatePushRegistration(ctx, e, s); err != nil {
		return "", err
	}
	if e.Kind == event.KIND_VANISH {
		if err := t.vanish(ctx, e); err != nil {
			return "", err
		}
		outcome = "ok"
		return "", nil
	}
	if e.Kind == event.KIND_REPORT {
		reason, err := t.publishReport(ctx, e)
		if err == nil {
			outcome = "ok"
		}
		return reason, err
	}
	plan, metadata, err := t.prepareClientEvent(ctx, e, s, now)
	if err != nil {
		return "", err
	}
	reason, err := t.persistClientEvent(ctx, e, plan.options)
	if errors.Is(err, storage.ErrDuplicate) {
		outcome = "duplicate"
		return storage.ErrDuplicate.Error(), nil
	}
	if err != nil {
		return "", err
	}
	t.afterStoredEvent(ctx, e, plan.followups)
	// Stage Git metadata before acknowledging the event. This closes the
	// publish-ACK/receive-pack race: the signed pending refs and hook exist
	// before a client can push objects for the state.
	if plan.gitMetadata {
		if err := t.git.CommitAfterStoreNoNotify(ctx, e, plan.repository); err != nil {
			return "", err
		}
	}
	if metadata.changed {
		t.mu.Lock()
		t.policy = metadata.next
		t.mu.Unlock()
	}
	if err := t.records.NotePresence(ctx, e.PubKey, now); err != nil {
		t.app.telemetry.Logger().Error("publish presence", "error", err)
	}
	if e.PubKey == t.Policy().Owner {
		if err := t.records.Heartbeat(ctx, e.PubKey, now); err != nil {
			t.app.telemetry.Logger().Error("owner heartbeat", "error", err)
		}
	}
	outcome = "ok"
	return reason, nil
}

// roomScope names the room an event is addressed to when that room is not
// the tenant's own group, which keeps its established handling.
func (t *Tenant) roomScope(e event.Event) string {
	if e.Kind == event.KIND_MARMOT_GROUP {
		return ""
	}
	if id := event.Tag(e, "h"); id != "" && id != t.meta.Name {
		return id
	}
	return ""
}

func (t *Tenant) vanish(ctx context.Context, e event.Event) error {
	for _, tag := range e.Tags {
		if len(tag) > 1 && tag[0] == "relay" && (tag[1] == "ALL_RELAYS" || strings.TrimSuffix(tag[1], "/") == strings.TrimSuffix(t.RelayURL(), "/")) {
			return t.store.Vanish(ctx, e.PubKey, e.CreatedAt)
		}
	}
	return errors.New("invalid: vanish request does not name this relay")
}

func parsePinTags(tags [][]string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, tag := range tags {
		if len(tag) < 2 || (tag[0] != "e" && tag[0] != "a") {
			continue
		}
		ref := tag[1]
		if tag[0] == "e" && (len(ref) != 64 || !hexLower(ref)) {
			continue
		}
		if tag[0] == "a" && !strings.Contains(ref, ":") {
			continue
		}
		if !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	if len(out) > 20 {
		return nil, errors.New("invalid: pin list has more than 20 entries")
	}
	return out, nil
}

func hexLower(value string) bool {
	for _, c := range value {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func (t *Tenant) Query(ctx context.Context, filters []event.Filter, s relay.Session) ([]event.Event, error) {
	ctx, done, admissionErr := t.beginOperation(ctx)
	if admissionErr != nil {
		return nil, admissionErr
	}
	defer done()
	if _, err := t.gate.Read(ctx, filters, s); err != nil {
		return nil, err
	}
	result := []event.Event{}
	seen := make(map[string]bool)
	for _, f := range filters {
		invite, includeInvite, err := t.nip43Invite(ctx, f, s)
		if err != nil {
			return nil, err
		}
		if includeInvite && !seen[invite.ID] {
			seen[invite.ID] = true
			result = append(result, invite)
		}
		queryFilter, queryStored := withoutNIP43Invite(f)
		if !queryStored {
			continue
		}
		rows, err := t.store.Query(ctx, queryFilter, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{PubKeys: s.PubKeys}})
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
	ctx, done, admissionErr := t.beginOperation(ctx)
	if admissionErr != nil {
		return nil, admissionErr
	}
	defer done()
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
	ctx, done, admissionErr := t.beginOperation(ctx)
	if admissionErr != nil {
		return nil, admissionErr
	}
	defer done()
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
	ctx, done, admissionErr := t.beginOperation(ctx)
	if admissionErr != nil {
		return nil, nil, admissionErr
	}
	defer done()
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
	t.policyWrite.Lock()
	defer t.policyWrite.Unlock()
	next, err := policy.Patch(t.Policy(), patch)
	if err != nil {
		return next, err
	}
	return next, t.persistPolicy(ctx, next)
}

func (t *Tenant) applyPolicy(ctx context.Context, next policy.Policy) error {
	t.policyWrite.Lock()
	defer t.policyWrite.Unlock()
	return t.persistPolicy(ctx, next)
}

func (t *Tenant) validatePolicyTransition(next policy.Policy) error {
	previous := t.Policy()
	if previous.Features.Grasp08 && previous.Reads == "members" && (!next.Features.Grasp08 || next.Reads != "members") {
		return errors.New("blocked: opening a private GRASP-08 tenant requires an explicit data migration")
	}
	return nil
}

func (t *Tenant) persistPolicy(ctx context.Context, next policy.Policy) error {
	previous := t.Policy()
	if err := t.validatePolicyTransition(next); err != nil {
		return err
	}
	err := t.store.WithTx(ctx, func(tx *sql.Tx) error {
		if next.Owner != previous.Owner {
			if err := t.community.ApplyOwnerTx(ctx, tx, previous.Owner, next.Owner); err != nil {
				return err
			}
			if err := storage.AddIntents(ctx, tx, []storage.Intent{{Kind: "catalog-owner", EventID: next.Owner + time.Now().Format(time.RFC3339Nano), Target: t.meta.ID, Payload: next.Owner}}, time.Now().Unix()); err != nil {
				return err
			}
		}
		return storage.PutSetting(ctx, tx, "policy", next)
	})
	if err != nil {
		return err
	}
	t.community.SetOwner(next.Owner)
	t.replacePolicy(next)
	return nil
}

package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/FelineStateMachine/tinyrelay/internal/catalog"
	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/telemetry"
	"github.com/FelineStateMachine/tinyrelay/internal/templates"
)

// Config supplies process-level paths, public addresses, transport limits and
// operator permissions. Tenant policy supplies each relay's access rules.
// DataDir is required; DefaultTenant defaults to "main". PublicURL, when set,
// is an absolute HTTP or HTTPS address used for externally visible URLs.
type Config struct {
	DataDir            string
	PublicURL          string
	DefaultTenant      string
	Version            string
	Revision           string
	Telemetry          telemetry.Config
	MaxMessageBytes    int64
	MaxPendingBytes    int
	AllowPrivateRelays bool
	// ProvisionOwners controls browser-created tenants. An empty list closes
	// public provisioning while direct operator creation remains available.
	ProvisionOwners []string
	DiscoveryRelays []string
	// PushCallbackOrigins is the operator-approved HTTPS origin allowlist for
	// host-wide push registrations. Tenant policy PushCallbacks is a separate
	// owner-controlled allowlist; both approvals are required.
	PushCallbackOrigins []string
	// PeerMonitor configures NIP-66 liveness probes for its listed peers.
	PeerMonitor *PeerMonitorConfig
}

type CreateOptions = catalog.CreateOptions

// App owns the catalog, active tenants and process-wide service lifecycle.
// New constructs it, Start starts serving work, and Close releases resources.
// An HTTP server uses App as its handler.
type App struct {
	cfg         Config
	catalog     *catalog.Catalog
	telemetry   *telemetry.Telemetry
	peerMonitor *PeerMonitor
	mu          sync.Mutex
	tenants     map[string]*Tenant
	closed      bool
	lifecycle   lifecycleState
}

// New opens catalog and telemetry resources and attempts to resume incomplete
// tenant creation. The caller owns the returned App and calls Close to release
// it. Start begins the serving lifecycle.
func New(ctx context.Context, cfg Config) (*App, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("data directory is required")
	}
	if cfg.DefaultTenant == "" {
		cfg.DefaultTenant = "main"
	}
	if cfg.PublicURL != "" {
		u, err := url.Parse(cfg.PublicURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, errors.New("public URL must be an absolute http or https URL")
		}
	}
	cat, err := catalog.Open(ctx, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.Telemetry.Version = cfg.Version
	cfg.Telemetry.Revision = cfg.Revision
	metrics, err := telemetry.New(ctx, cfg.Telemetry)
	if err != nil {
		return nil, errors.Join(err, cat.Close())
	}
	var peerMonitor *PeerMonitor
	if cfg.PeerMonitor != nil {
		peerMonitor, err = NewPeerMonitor(*cfg.PeerMonitor)
		if err != nil {
			return nil, errors.Join(err, cat.Close(), metrics.Close(ctx))
		}
	}
	app := &App{cfg: cfg, catalog: cat, telemetry: metrics, peerMonitor: peerMonitor, tenants: make(map[string]*Tenant)}
	if err := metrics.Register(app.QueueCollector()); err != nil {
		return nil, errors.Join(err, cat.Close(), metrics.Close(ctx))
	}
	if err := app.RecoverCreating(ctx); err != nil {
		metrics.Logger().Error("recover unfinished tenant creation", "error", err)
	}
	return app, nil
}

func (a *App) Catalog() *catalog.Catalog { return a.catalog }
func (a *App) Diagnostics() http.Handler { return a.telemetry.Handler() }
func (a *App) PeerMonitor() *PeerMonitor { return a.peerMonitor }

// ConfigurePeerPublication binds the explicitly selected tenant to the
// opt-in NIP-66 publisher. The CLI operator is the authorization boundary;
// tenant selection is required so a private tenant is never implicit.
func (a *App) ConfigurePeerPublication(ctx context.Context, tenantName string) error {
	if a.peerMonitor == nil {
		return errors.New("daemon: peer monitor is not configured")
	}
	if strings.TrimSpace(tenantName) == "" {
		return errors.New("daemon: peer publication tenant is required")
	}
	meta, err := a.catalog.GetByName(ctx, tenantName)
	if err != nil {
		return err
	}
	a.lifecycle.mu.Lock()
	base, started := a.lifecycle.baseURL, a.lifecycle.started
	a.lifecycle.mu.Unlock()
	if !started {
		return errors.New("daemon: start the app before configuring peer publication")
	}
	publicURL := base
	if meta.Name != a.cfg.DefaultTenant {
		publicURL += "/r/" + meta.Name
	}
	t, err := a.tenant(ctx, meta, publicURL)
	if err != nil {
		return err
	}
	a.peerMonitor.SetPublisher(t.PublishPeerStatus)
	return nil
}

// PeerMonitorRun runs configured NIP-66 probes under the caller's lifecycle.
// Publication remains opt-in in PeerMonitorConfig and is only a capability
// status until an operator supplies a signed publication path.
func (a *App) PeerMonitorRun(ctx context.Context) error {
	if a.peerMonitor == nil {
		return errors.New("daemon: peer monitor is not configured")
	}
	return a.peerMonitor.Run(ctx)
}

// Create provisions a tenant from a template, initializes its durable state
// and marks it ready. A running App also starts its services. The returned
// metadata identifies the creation record even if a later setup step fails;
// RecoverCreating resumes incomplete creation from that durable record.
func (a *App) Create(ctx context.Context, opts CreateOptions) (catalog.Tenant, error) {
	if template, ok := templates.Find(opts.Template); ok && template.Source == "required" && strings.TrimSpace(opts.Source) == "" {
		return catalog.Tenant{}, fmt.Errorf("template %q requires a source relay", opts.Template)
	}
	p, err := templates.ApplyTemplate(opts.Template, opts.Owner)
	if err != nil {
		return catalog.Tenant{}, err
	}
	meta, err := a.catalog.Create(ctx, opts)
	if err != nil {
		return meta, err
	}
	store, err := storage.Open(ctx, meta.Paths.Database)
	if err != nil {
		return meta, err
	}
	if err := initializeTenant(ctx, store, p, opts.Template); err != nil {
		return meta, errors.Join(err, store.Close())
	}
	if err := store.Close(); err != nil {
		return meta, err
	}
	if err := a.AfterCreate(ctx, meta, opts); err != nil {
		return meta, err
	}
	if err := a.catalog.MarkReady(ctx, meta.ID); err != nil {
		return meta, err
	}
	meta.Status = catalog.StatusReady
	if err := a.startCreatedTenant(ctx, meta); err != nil {
		return meta, fmt.Errorf("relay created but services could not start: %w", err)
	}
	return meta, nil
}

func initializeTenant(ctx context.Context, s *storage.Store, p policy.Policy, name string) error {
	if _, err := community.New(ctx, s, p.Owner); err != nil {
		return err
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		if err := storage.PutSetting(ctx, tx, "policy", p); err != nil {
			return err
		}
		communityConfig := community.ConfigSnapshot{}
		for _, rule := range []struct {
			kinds []int
			name  string
		}{{p.AllowedKinds, "allow"}, {p.BlockedKinds, "block"}} {
			for _, kind := range rule.kinds {
				communityConfig.KindRules = append(communityConfig.KindRules, community.ConfigKindRule{Kind: kind, Rule: rule.name})
			}
		}
		for _, r := range p.Retention {
			kind := r.Kind
			communityConfig.Retention = append(communityConfig.Retention, community.ConfigRetention{Kind: &kind, Days: r.Days})
		}
		if err := community.ApplyConfigTx(ctx, tx, communityConfig, false, false, false, false, true, true); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS config_connections(position INTEGER PRIMARY KEY,value TEXT NOT NULL)"); err != nil {
			return err
		}
		template, ok := templates.Find(name)
		if !ok {
			return fmt.Errorf("template %q disappeared", name)
		}
		connections := template.Connections
		if connections == nil {
			connections = []string{"notes", "find-me", "group"}
		}
		for i, name := range connections {
			visibility := "public"
			for _, connection := range templates.Connections() {
				if connection.Name == name {
					visibility = connection.Visibility
					break
				}
			}
			b, err := json.Marshal(map[string]string{"template": name, "visibility": visibility})
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO config_connections(position,value) VALUES(?,?)", i, string(b)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (a *App) tenant(ctx context.Context, meta catalog.Tenant, publicURL string) (*Tenant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, errors.New("daemon is shutting down")
	}
	current, err := a.catalog.GetByID(ctx, meta.ID)
	if err != nil {
		return nil, err
	}
	if current.Status != catalog.StatusReady {
		return nil, errors.New("relay is not ready")
	}
	meta = current
	if t := a.tenants[meta.ID]; t != nil {
		return t, nil
	}
	s, err := storage.Open(ctx, meta.Paths.Database)
	if err != nil {
		return nil, err
	}
	s.SetObserver(a.telemetry)
	var p policy.Policy
	err = s.GetSetting(ctx, "policy", &p)
	if errors.Is(err, sql.ErrNoRows) {
		p, err = templates.ApplyTemplate(meta.Template, meta.Owner)
	}
	if err != nil {
		return nil, errors.Join(err, s.Close())
	}
	t, err := newTenant(ctx, tenantConfig{app: a, meta: meta, store: s, policy: p, publicURL: publicURL})
	if err != nil {
		return nil, errors.Join(err, s.Close())
	}
	a.tenants[meta.ID] = t
	return t, nil
}

// Close stops catalog reconciliation and tenant services, then closes shared
// resources and releases the process lock. It joins cleanup errors and returns
// nil on subsequent calls. The caller shuts down its HTTP servers separately.
func (a *App) Close(ctx context.Context) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	a.mu.Unlock()
	lifecycleErr := a.stopLifecycle()
	a.mu.Lock()
	var tenants []*Tenant
	for _, t := range a.tenants {
		tenants = append(tenants, t)
	}
	a.mu.Unlock()
	errs := []error{lifecycleErr}
	for _, t := range tenants {
		errs = append(errs, t.Close(ctx))
	}
	errs = append(errs, a.catalog.Close(), a.telemetry.Close(ctx), a.releaseProcessLock())
	return errors.Join(errs...)
}

// ServeHTTP resolves the request's tenant or hosted site and delegates to its
// handler. Health and relay discovery routes are served at the process level.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/relays" && a.ServeLanding(w, r) {
		return
	}
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		a.healthHTTP(w, r)
		return
	}
	ctx, finish := a.telemetry.Start(r.Context(), "http")
	r = r.WithContext(ctx)
	if meta, label, isSite, siteErr := a.resolveHostedSite(r); isSite {
		if siteErr != nil {
			finish("error")
			http.NotFound(w, r)
			return
		}
		if meta.Status != catalog.StatusReady {
			finish("unavailable")
			http.Error(w, "relay is "+string(meta.Status), http.StatusServiceUnavailable)
			return
		}
		t, err := a.tenant(ctx, meta, a.publicURL(r, ""))
		if err != nil {
			finish("error")
			http.Error(w, "relay could not be opened", http.StatusServiceUnavailable)
			return
		}
		if t.sites == nil {
			finish("error")
			http.NotFound(w, r)
			return
		}
		ctx, done, err := t.beginOperation(r.Context())
		if err != nil {
			finish("unavailable")
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer done()
		r = r.WithContext(ctx)
		t.sites.HandlerForLabel(label).ServeHTTP(w, r)
		finish("ok")
		return
	}
	meta, prefix, err := a.resolve(r)
	if err != nil {
		finish("error")
		if errors.Is(err, catalog.ErrNotFound) && r.Method == http.MethodGet && r.URL.Path == "/" && !strings.Contains(r.Header.Get("Accept"), "application/nostr+json") && !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && a.ServeLanding(w, r) {
			return
		}
		http.Error(w, "relay not found", http.StatusNotFound)
		return
	}
	if meta.Status != catalog.StatusReady {
		finish("unavailable")
		http.Error(w, "relay is "+string(meta.Status), http.StatusServiceUnavailable)
		return
	}
	publicURL := a.publicURL(r, prefix)
	t, err := a.tenant(ctx, meta, publicURL)
	if err != nil {
		finish("error")
		a.telemetry.Logger().Error("open tenant", "error", err)
		http.Error(w, "relay could not be opened", http.StatusServiceUnavailable)
		return
	}
	if prefix != "" {
		r = r.Clone(ctx)
		u := *r.URL
		r.URL = &u
		r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
	}
	t.ServeHTTP(w, r)
	finish("ok")
}

func (a *App) resolve(r *http.Request) (catalog.Tenant, string, error) {
	if strings.HasPrefix(r.URL.Path, "/r/") {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/r/"), "/", 2)
		meta, err := a.catalog.GetByName(r.Context(), parts[0])
		return meta, "/r/" + parts[0], err
	}
	host := r.Host
	if hostname, _, err := net.SplitHostPort(host); err == nil {
		host = hostname
	}
	meta, err := a.catalog.ResolveHost(r.Context(), host)
	if err == nil {
		return meta, "", nil
	}
	if !errors.Is(err, catalog.ErrNotFound) {
		return meta, "", err
	}
	meta, err = a.catalog.GetByName(r.Context(), a.cfg.DefaultTenant)
	return meta, "", err
}

func (a *App) publicURL(r *http.Request, prefix string) string {
	if a.cfg.PublicURL != "" {
		return strings.TrimSuffix(a.cfg.PublicURL, "/") + prefix
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s%s", scheme, r.Host, prefix)
}

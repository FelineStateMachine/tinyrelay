package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/catalog"
	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/templates"
)

// AfterCreate finishes the durable work for a catalog row still in creating.
// It is intentionally separate from Catalog.Create so callers can retry after
// a crash without creating a second tenant or losing the assigned owner.
func (a *App) AfterCreate(ctx context.Context, meta catalog.Tenant, opts CreateOptions) error {
	template, ok := templates.Find(meta.Template)
	if !ok {
		return fmt.Errorf("creation: template %q not found", meta.Template)
	}
	source := strings.TrimSpace(opts.Source)
	if source == "" {
		source = meta.Source
	}
	if template.Source == "required" && source == "" {
		return fmt.Errorf("creation: template %q requires a source relay", template.Name)
	}
	if source != "" && !validSource(source) {
		return errors.New("creation: source must be an http, https, ws, or wss relay")
	}
	store, err := storage.Open(ctx, meta.Paths.Database)
	if err != nil {
		return fmt.Errorf("creation: open tenant store: %w", err)
	}
	defer store.Close()
	if err := initializeTenantRetryable(ctx, store, meta, template); err != nil {
		return err
	}
	if source == "" && template.Name == "inbox" {
		return seedInboxDiscoveryJob(ctx, store, meta, template)
	}
	if source == "" || !acceptsSource(template.Name) {
		return nil
	}
	return seedSourceJob(ctx, store, meta, template, source)
}

func seedInboxDiscoveryJob(ctx context.Context, store *storage.Store, meta catalog.Tenant, template templates.Template) error {
	service, err := replication.NewService(replication.Config{Store: store, DataDir: meta.Paths.Root})
	if err != nil {
		return fmt.Errorf("creation: initialize replication: %w", err)
	}
	filterObject := map[string]any{"kinds": template.AllowKinds, "#p": []string{meta.Owner}}
	raw, err := json.Marshal(filterObject)
	if err != nil {
		return err
	}
	spec := replication.JobSpec{ID: "source-" + meta.ID, Kind: replication.JobPull, Filter: string(raw), Every: 1, DiscoverPubKey: meta.Owner}
	if err := service.AddJob(ctx, spec); err != nil {
		return fmt.Errorf("creation: seed inbox discovery job: %w", err)
	}
	return nil
}

// RecoverCreating retries every interrupted creation and makes only tenants
// whose policy and source jobs were durably initialized ready.
func (a *App) RecoverCreating(ctx context.Context) error {
	items, err := a.catalog.List(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, meta := range items {
		if meta.Status != catalog.StatusCreating {
			continue
		}
		if err := a.AfterCreate(ctx, meta, CreateOptions{Source: meta.Source, Owner: meta.Owner, Template: meta.Template}); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", meta.Name, err))
			continue
		}
		if err := a.catalog.MarkReady(ctx, meta.ID); err != nil {
			failures = append(failures, fmt.Errorf("%s: mark ready: %w", meta.Name, err))
		}
	}
	return errors.Join(failures...)
}

func initializeTenantRetryable(ctx context.Context, store *storage.Store, meta catalog.Tenant, template templates.Template) error {
	p, err := templates.ApplyTemplate(template.Name, meta.Owner)
	if err != nil {
		return err
	}
	if _, err := community.New(ctx, store, p.Owner); err != nil {
		return err
	}
	err = store.WithTx(ctx, func(tx *sql.Tx) error {
		if err := storage.PutSetting(ctx, tx, "policy", p); err != nil {
			return err
		}
		for _, rule := range []struct {
			kinds []int
			name  string
		}{{p.AllowedKinds, "allow"}, {p.BlockedKinds, "block"}} {
			for _, kind := range rule.kinds {
				if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO community_kind_rules(kind,rule) VALUES(?,?)", kind, rule.name); err != nil {
					return err
				}
			}
		}
		for _, retention := range p.Retention {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO community_retention(kind,days) VALUES(?,?)", retention.Kind, retention.Days); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS config_connections(position INTEGER PRIMARY KEY,value TEXT NOT NULL)"); err != nil {
			return err
		}
		for index, connection := range template.Connections {
			raw, err := json.Marshal(map[string]string{"template": connection, "visibility": "public"})
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO config_connections(position,value) VALUES(?,?)", index, string(raw)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("creation: initialize policy: %w", err)
	}
	return nil
}

func seedSourceJob(ctx context.Context, store *storage.Store, meta catalog.Tenant, template templates.Template, source string) error {
	service, err := replication.NewService(replication.Config{Store: store, DataDir: meta.Paths.Root})
	if err != nil {
		return fmt.Errorf("creation: initialize replication: %w", err)
	}
	filter := ""
	if len(template.AllowKinds) > 0 {
		filterObject := map[string]any{"kinds": template.AllowKinds}
		if template.Name == "inbox" {
			filterObject["#p"] = []string{meta.Owner}
		}
		raw, err := json.Marshal(filterObject)
		if err != nil {
			return err
		}
		filter = string(raw)
	}
	kind := replication.JobPull
	every := template.EveryHours
	if template.Name == "inbox" {
		every = 1
	}
	if template.Name == "outbox" {
		kind = replication.JobMirror
		if every == 0 {
			every = 24
		}
	}
	spec := replication.JobSpec{ID: "source-" + meta.ID, Kind: kind, Relays: []string{sourceRelayURL(source)}, Filter: filter, Every: every}
	if err := service.AddJob(ctx, spec); err != nil {
		return fmt.Errorf("creation: seed source job: %w", err)
	}
	return nil
}

func validSource(source string) bool {
	u, err := url.Parse(source)
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "ws" || u.Scheme == "wss") && u.User == nil
}

func acceptsSource(name string) bool {
	return name == "search" || name == "articles" || name == "inbox" || name == "outbox"
}

func sourceRelayURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	return u.String()
}

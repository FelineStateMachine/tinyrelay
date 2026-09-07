package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/catalog"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
)

// siteBase returns the tenant-scoped site hostname. A tenant named alice on
// relay.example has sites at <site>.alice.relay.example, keeping identical
// site labels on different tenants isolated.
func (a *App) siteBase(meta catalog.Tenant) string {
	base := "localhost"
	if a.cfg.PublicURL != "" {
		if u, err := url.Parse(a.cfg.PublicURL); err == nil && u.Hostname() != "" {
			base = u.Hostname()
		}
	}
	if ip := net.ParseIP(base); ip != nil {
		return meta.Name + "." + base
	}
	return meta.Name + "." + strings.TrimSuffix(strings.ToLower(base), ".")
}

// resolveHostedSite resolves a site host without opening a tenant. The bool
// distinguishes a relay host from a site host, while errors preserve catalog
// failures for callers that should not fall back to the default tenant.
func (a *App) resolveHostedSite(r *http.Request) (catalog.Tenant, string, bool, error) {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if strings.HasPrefix(r.URL.Path, "/r/") {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/r/"), "/")
		if len(parts) > 0 && parts[0] != "" {
			meta, err := a.catalog.GetByName(r.Context(), parts[0])
			return meta, "", false, err
		}
	}
	if mapping, err := a.catalog.ResolveHostMapping(r.Context(), host); err == nil {
		meta, getErr := a.catalog.GetByID(r.Context(), mapping.TenantID)
		if getErr != nil {
			return catalog.Tenant{}, "", false, getErr
		}
		if mapping.Site == "" {
			return meta, "", false, nil
		}
		if _, ok := sites.ParseSite(mapping.Site); !ok {
			return catalog.Tenant{}, mapping.Site, true, errors.New("invalid mapped site label")
		}
		return meta, mapping.Site, true, nil
	} else if !errors.Is(err, catalog.ErrHostMappingNotFound) {
		return catalog.Tenant{}, "", false, err
	}
	base := strings.TrimSuffix(strings.ToLower(a.siteBaseHost()), ".")
	suffix := "." + base
	if !strings.HasSuffix(host, suffix) {
		return catalog.Tenant{}, "", false, catalog.ErrNotFound
	}
	relative := strings.TrimSuffix(strings.TrimSuffix(host, suffix), ".")
	parts := strings.Split(relative, ".")
	if len(parts) == 1 {
		if _, ok := sites.ParseSite(relative); !ok {
			return catalog.Tenant{}, "", false, catalog.ErrNotFound
		}
		meta, err := a.catalog.GetByName(r.Context(), a.cfg.DefaultTenant)
		return meta, relative, true, err
	}
	if len(parts) < 2 {
		return catalog.Tenant{}, "", false, catalog.ErrNotFound
	}
	label := strings.Join(parts[:len(parts)-1], ".")
	if _, ok := sites.ParseSite(label); !ok {
		return catalog.Tenant{}, label, true, catalog.ErrNotFound
	}
	tenantName := parts[len(parts)-1]
	meta, err := a.catalog.GetByName(r.Context(), tenantName)
	if err != nil {
		return catalog.Tenant{}, label, true, err
	}
	return meta, label, true, nil
}

func (a *App) siteBaseHost() string {
	if a.cfg.PublicURL == "" {
		return "localhost"
	}
	u, err := url.Parse(a.cfg.PublicURL)
	if err != nil || u.Hostname() == "" {
		return "localhost"
	}
	return u.Hostname()
}

// siteHostResolver is injected into a tenant's sites service. It only returns
// labels assigned to that tenant, preventing a custom host or namespace host
// from crossing tenant boundaries.
func (t *Tenant) siteHostResolver(ctx context.Context, host string) (string, error) {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if mapping, err := t.app.catalog.ResolveHostMapping(ctx, host); err == nil {
		if mapping.TenantID != t.meta.ID || mapping.Site == "" {
			return "", catalog.ErrHostMappingNotFound
		}
		return mapping.Site, nil
	} else if !errors.Is(err, catalog.ErrHostMappingNotFound) {
		return "", err
	}
	base := "." + strings.ToLower(t.app.siteBase(t.meta))
	if !strings.HasSuffix(host, base) {
		return "", catalog.ErrNotFound
	}
	label := strings.TrimSuffix(strings.TrimSuffix(host, base), ".")
	if _, ok := sites.ParseSite(label); !ok {
		return "", catalog.ErrNotFound
	}
	return label, nil
}

func (a *App) hostedSiteURL(meta catalog.Tenant, label string) string {
	if label == "" {
		return ""
	}
	return fmt.Sprintf("https://%s/%s", a.siteBase(meta), label)
}

package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// HostMapping is an operator-owned hostname routed to one tenant. Site is an
// optional NIP-5A label; an empty site routes the relay itself.
type HostMapping struct {
	TenantID  string    `json:"tenantId"`
	Host      string    `json:"host"`
	Site      string    `json:"site,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

var ErrHostMappingNotFound = errors.New("host mapping not found")

func (c *Catalog) AddHost(ctx context.Context, tenantID, host, site string) (HostMapping, error) {
	if err := contextError(ctx); err != nil {
		return HostMapping{}, err
	}
	normal, err := normalizeHost(host)
	if err != nil {
		return HostMapping{}, err
	}
	if tenantID == "" {
		return HostMapping{}, errors.New("tenant id is required")
	}
	if _, err := c.GetByID(ctx, tenantID); err != nil {
		return HostMapping{}, err
	}
	var primaryTenant string
	if err := c.db.QueryRowContext(ctx, "SELECT id FROM tenants WHERE host=?", normal).Scan(&primaryTenant); err == nil {
		return HostMapping{}, ErrHostTaken
	} else if !errors.Is(err, sql.ErrNoRows) {
		return HostMapping{}, fmt.Errorf("check primary host: %w", err)
	}
	now := time.Now().UTC()
	_, err = c.db.ExecContext(ctx, `INSERT INTO tenant_hosts(tenant_id,host,site,status,created_at,updated_at) VALUES(?,?,?,?,?,?)`, tenantID, normal, site, "configured", now.UnixNano(), now.UnixNano())
	if err != nil {
		if isConstraint(err) {
			return HostMapping{}, ErrHostTaken
		}
		return HostMapping{}, fmt.Errorf("add host mapping: %w", err)
	}
	return HostMapping{TenantID: tenantID, Host: normal, Site: site, Status: "configured", CreatedAt: now, UpdatedAt: now}, nil
}

func (c *Catalog) SetHostSite(ctx context.Context, tenantID, host, site string) (HostMapping, error) {
	if err := contextError(ctx); err != nil {
		return HostMapping{}, err
	}
	normal, err := normalizeHost(host)
	if err != nil {
		return HostMapping{}, err
	}
	now := time.Now().UTC()
	result, err := c.db.ExecContext(ctx, `UPDATE tenant_hosts SET site=?,updated_at=? WHERE tenant_id=? AND host=?`, site, now.UnixNano(), tenantID, normal)
	if err != nil {
		return HostMapping{}, fmt.Errorf("set host site: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return HostMapping{}, ErrHostMappingNotFound
	}
	return c.hostMapping(ctx, tenantID, normal)
}

func (c *Catalog) RemoveHost(ctx context.Context, tenantID, host string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	normal, err := normalizeHost(host)
	if err != nil {
		return err
	}
	result, err := c.db.ExecContext(ctx, `DELETE FROM tenant_hosts WHERE tenant_id=? AND host=?`, tenantID, normal)
	if err != nil {
		return fmt.Errorf("remove host mapping: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrHostMappingNotFound
	}
	return nil
}

func (c *Catalog) ListHosts(ctx context.Context, tenantID string) ([]HostMapping, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	rows, err := c.db.QueryContext(ctx, `SELECT tenant_id,host,site,status,created_at,updated_at FROM tenant_hosts WHERE tenant_id=? ORDER BY host`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list host mappings: %w", err)
	}
	defer rows.Close()
	result := []HostMapping{}
	for rows.Next() {
		mapping, scanErr := scanHostMapping(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, mapping)
	}
	return result, rows.Err()
}

func (c *Catalog) ResolveHostMapping(ctx context.Context, host string) (HostMapping, error) {
	if err := contextError(ctx); err != nil {
		return HostMapping{}, err
	}
	normal, err := normalizeHost(host)
	if err != nil {
		return HostMapping{}, err
	}
	return c.hostMapping(ctx, "", normal)
}

func (c *Catalog) hostMapping(ctx context.Context, tenantID, host string) (HostMapping, error) {
	query := `SELECT tenant_id,host,site,status,created_at,updated_at FROM tenant_hosts WHERE host=?`
	args := []any{host}
	if tenantID != "" {
		query += " AND tenant_id=?"
		args = append(args, tenantID)
	}
	row := c.db.QueryRowContext(ctx, query, args...)
	mapping, err := scanHostMapping(row)
	if errors.Is(err, sql.ErrNoRows) {
		return HostMapping{}, ErrHostMappingNotFound
	}
	if err != nil {
		return HostMapping{}, fmt.Errorf("get host mapping: %w", err)
	}
	return mapping, nil
}

func scanHostMapping(row scanner) (HostMapping, error) {
	var m HostMapping
	var created, updated int64
	if err := row.Scan(&m.TenantID, &m.Host, &m.Site, &m.Status, &created, &updated); err != nil {
		return HostMapping{}, err
	}
	m.CreatedAt, m.UpdatedAt = time.Unix(0, created).UTC(), time.Unix(0, updated).UTC()
	return m, nil
}

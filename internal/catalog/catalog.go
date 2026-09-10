package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

type Status string

const (
	// StatusCreating identifies a tenant whose directories or services are
	// still being initialized.
	StatusCreating Status = "creating"
	// StatusReady identifies a tenant available for normal service.
	StatusReady Status = "ready"
	// StatusDisabled identifies a tenant stopped by the operator and eligible
	// for reactivation or deletion.
	StatusDisabled Status = "disabled"
	// StatusDeleting identifies a tenant whose deletion intent is recorded.
	StatusDeleting Status = "deleting"
)

var (
	ErrNotFound          = errors.New("catalog item not found")
	ErrNameTaken         = errors.New("tenant name is already in use")
	ErrHostTaken         = errors.New("hostname is already in use")
	ErrOwnerMismatch     = errors.New("current tenant owner does not match")
	ErrInvalidTransition = errors.New("invalid tenant status transition")
	ErrClosed            = errors.New("catalog is closed")
)

type CreateOptions struct {
	// Name is the operator-visible unique tenant name.
	Name string
	// Owner is the tenant owner's public key.
	Owner string
	// Template identifies the configuration template used during setup.
	Template string
	// Source records where the tenant configuration came from.
	Source string
}

// Paths contains the private filesystem locations assigned to a tenant.
type Paths struct {
	Root     string
	Database string
	Blobs    string
	Git      string
}

type Tenant struct {
	ID        string
	Name      string
	Owner     string
	Template  string
	Source    string
	Status    Status
	Host      string
	CreatedAt time.Time
	UpdatedAt time.Time
	Paths     Paths
}

type Catalog struct {
	db       *sql.DB
	dataRoot string
	mu       sync.RWMutex
	closed   bool
}

func Open(ctx context.Context, dataRoot string) (*Catalog, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if dataRoot == "" {
		return nil, errors.New("catalog data root is empty")
	}
	if err := os.MkdirAll(dataRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create catalog data root: %w", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dataRoot, "catalog.db"))
	if err != nil {
		return nil, fmt.Errorf("open catalog database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	cat := &Catalog{db: db, dataRoot: dataRoot}
	if err := cat.configure(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return cat, nil
}

func (c *Catalog) configure(ctx context.Context) error {
	statements := []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = FULL",
		"PRAGMA journal_mode = WAL",
		`CREATE TABLE IF NOT EXISTS tenants (
			id TEXT PRIMARY KEY, name TEXT NOT NULL COLLATE NOCASE UNIQUE,
			owner TEXT NOT NULL, template TEXT NOT NULL, source TEXT NOT NULL DEFAULT '', status TEXT NOT NULL,
			host TEXT COLLATE NOCASE UNIQUE, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
			CHECK(status IN ('creating','ready','disabled','deleting'))
		)`,
		`CREATE TABLE IF NOT EXISTS tenant_hosts (
			tenant_id TEXT NOT NULL, host TEXT PRIMARY KEY COLLATE NOCASE,
			site TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'configured',
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
			FOREIGN KEY(tenant_id) REFERENCES tenants(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS tenant_deletions (
			id TEXT PRIMARY KEY, root TEXT NOT NULL, staging TEXT NOT NULL,
			started_at INTEGER NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := c.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure catalog: %w", err)
		}
	}
	if err := c.ensureSourceColumn(ctx); err != nil {
		return err
	}
	if err := c.migrateTenantStatus(ctx); err != nil {
		return err
	}
	if err := c.recoverDeletions(ctx); err != nil {
		return err
	}
	return nil
}

func (c *Catalog) migrateTenantStatus(ctx context.Context) error {
	var definition string
	if err := c.db.QueryRowContext(ctx, "SELECT sql FROM sqlite_schema WHERE type='table' AND name='tenants'").Scan(&definition); err != nil {
		return fmt.Errorf("inspect tenant status schema: %w", err)
	}
	if strings.Contains(strings.ToLower(definition), "'deleting'") {
		return nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tenant status migration: %w", err)
	}
	defer tx.Rollback()
	statements := []string{
		"ALTER TABLE tenants RENAME TO tenants_legacy",
		`CREATE TABLE tenants (
			id TEXT PRIMARY KEY, name TEXT NOT NULL COLLATE NOCASE UNIQUE,
			owner TEXT NOT NULL, template TEXT NOT NULL, source TEXT NOT NULL DEFAULT '', status TEXT NOT NULL,
			host TEXT COLLATE NOCASE UNIQUE, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
			CHECK(status IN ('creating','ready','disabled','deleting'))
		)`,
		"INSERT INTO tenants(id,name,owner,template,source,status,host,created_at,updated_at) SELECT id,name,owner,template,source,status,host,created_at,updated_at FROM tenants_legacy",
		"DROP TABLE tenants_legacy",
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate tenant status schema: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tenant status migration: %w", err)
	}
	return nil
}

func (c *Catalog) ensureSourceColumn(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, "PRAGMA table_info(tenants)")
	if err != nil {
		return fmt.Errorf("inspect tenant schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primary int
		var name, typ string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primary); err != nil {
			return err
		}
		if name == "source" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, "ALTER TABLE tenants ADD COLUMN source TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("migrate tenant source: %w", err)
	}
	return nil
}

func (c *Catalog) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.db.Close()
}

func (c *Catalog) Create(ctx context.Context, options CreateOptions) (Tenant, error) {
	if err := contextError(ctx); err != nil {
		return Tenant{}, err
	}
	name, err := normalizeName(options.Name)
	if err != nil {
		return Tenant{}, err
	}
	owner, err := normalizeOwner(options.Owner)
	if err != nil {
		return Tenant{}, err
	}
	template, err := normalizeTemplate(options.Template)
	if err != nil {
		return Tenant{}, err
	}
	source, err := normalizeSource(options.Source)
	if err != nil {
		return Tenant{}, err
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	_, err = c.db.ExecContext(ctx, `INSERT INTO tenants
		(id,name,owner,template,source,status,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		id, name, owner, template, source, StatusCreating, now.UnixNano(), now.UnixNano())
	if err != nil {
		if isConstraint(err) {
			return Tenant{}, ErrNameTaken
		}
		return Tenant{}, fmt.Errorf("insert tenant: %w", err)
	}
	paths := c.tenantPaths(id)
	if err := makeTenantDirs(paths); err != nil {
		return Tenant{}, fmt.Errorf("prepare tenant %q: %w", id, err)
	}
	return Tenant{ID: id, Name: name, Owner: owner, Template: template, Source: source,
		Status: StatusCreating, CreatedAt: now, UpdatedAt: now, Paths: paths}, nil
}

func (c *Catalog) MarkReady(ctx context.Context, id string) error {
	return c.transition(ctx, id, StatusCreating, StatusReady)
}

func (c *Catalog) Disable(ctx context.Context, id string) error {
	return c.transition(ctx, id, StatusReady, StatusDisabled)
}

func (c *Catalog) Enable(ctx context.Context, id string) error {
	return c.transition(ctx, id, StatusDisabled, StatusReady)
}

func (c *Catalog) UpdateOwner(ctx context.Context, id, owner string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	owner, err := normalizeOwner(owner)
	if err != nil {
		return err
	}
	result, err := c.db.ExecContext(ctx, "UPDATE tenants SET owner=?,updated_at=? WHERE id=?", owner, time.Now().UTC().UnixNano(), id)
	if err != nil {
		return fmt.Errorf("update tenant owner: %w", err)
	}
	return changedOrNotFound(result)
}

// TransferOwner changes ownership only when the expected current owner still holds it.
func (c *Catalog) TransferOwner(ctx context.Context, id, currentOwner, nextOwner string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	currentOwner, err := normalizeOwner(currentOwner)
	if err != nil {
		return err
	}
	nextOwner, err = normalizeOwner(nextOwner)
	if err != nil {
		return err
	}
	result, err := c.db.ExecContext(ctx, "UPDATE tenants SET owner=?,updated_at=? WHERE id=? AND owner=?", nextOwner, time.Now().UTC().UnixNano(), id, currentOwner)
	if err != nil {
		return fmt.Errorf("transfer tenant owner: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("check tenant owner transfer: %w", err)
	} else if count == 1 {
		return nil
	}
	var owner string
	if err := c.db.QueryRowContext(ctx, "SELECT owner FROM tenants WHERE id=?", id).Scan(&owner); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("check tenant owner: %w", err)
	}
	return ErrOwnerMismatch
}

func (c *Catalog) SetHost(ctx context.Context, id, host string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	host, err := normalizeHost(host)
	if err != nil {
		return err
	}
	var mappingTenant string
	err = c.db.QueryRowContext(ctx, "SELECT tenant_id FROM tenant_hosts WHERE host=?", host).Scan(&mappingTenant)
	if err == nil {
		return ErrHostTaken
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check host mapping: %w", err)
	}
	result, err := c.db.ExecContext(ctx, "UPDATE tenants SET host=?,updated_at=? WHERE id=?", host, time.Now().UTC().UnixNano(), id)
	if err != nil {
		if isConstraint(err) {
			return ErrHostTaken
		}
		return fmt.Errorf("set tenant host: %w", err)
	}
	return changedOrNotFound(result)
}

func (c *Catalog) ClearHost(ctx context.Context, id string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	result, err := c.db.ExecContext(ctx, "UPDATE tenants SET host=NULL,updated_at=? WHERE id=?", time.Now().UTC().UnixNano(), id)
	if err != nil {
		return fmt.Errorf("clear tenant host: %w", err)
	}
	return changedOrNotFound(result)
}

func (c *Catalog) GetByID(ctx context.Context, id string) (Tenant, error) {
	return c.get(ctx, "WHERE id=?", id)
}

func (c *Catalog) GetByName(ctx context.Context, name string) (Tenant, error) {
	name, err := normalizeName(name)
	if err != nil {
		return Tenant{}, err
	}
	return c.get(ctx, "WHERE name=?", name)
}

func (c *Catalog) ResolveHost(ctx context.Context, host string) (Tenant, error) {
	host, err := normalizeHost(host)
	if err != nil {
		return Tenant{}, err
	}
	ten, err := c.get(ctx, "WHERE host=?", host)
	if err == nil {
		return ten, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Tenant{}, err
	}
	var tenantID string
	if err := c.db.QueryRowContext(ctx, "SELECT tenant_id FROM tenant_hosts WHERE host=?", host).Scan(&tenantID); errors.Is(err, sql.ErrNoRows) {
		return Tenant{}, ErrNotFound
	} else if err != nil {
		return Tenant{}, fmt.Errorf("resolve host mapping: %w", err)
	}
	return c.GetByID(ctx, tenantID)
}

func (c *Catalog) List(ctx context.Context) ([]Tenant, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	rows, err := c.db.QueryContext(ctx, `SELECT id,name,owner,template,source,status,host,created_at,updated_at
		FROM tenants ORDER BY created_at,id`)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()
	var tenants []Tenant
	for rows.Next() {
		tenant, err := c.scanTenant(rows)
		if err != nil {
			return nil, err
		}
		tenants = append(tenants, tenant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	sort.SliceStable(tenants, func(i, j int) bool { return tenants[i].CreatedAt.Before(tenants[j].CreatedAt) })
	return tenants, nil
}

func (c *Catalog) transition(ctx context.Context, id string, from, to Status) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	result, err := c.db.ExecContext(ctx, "UPDATE tenants SET status=?,updated_at=? WHERE id=? AND status=?", to, time.Now().UTC().UnixNano(), id, from)
	if err != nil {
		return fmt.Errorf("transition tenant status: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("check tenant transition: %w", err)
	} else if count == 1 {
		return nil
	}
	var status Status
	if err := c.db.QueryRowContext(ctx, "SELECT status FROM tenants WHERE id=?", id).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("check tenant status: %w", err)
	}
	return fmt.Errorf("%w: %s to %s", ErrInvalidTransition, status, to)
}

func (c *Catalog) get(ctx context.Context, clause, value string) (Tenant, error) {
	if err := contextError(ctx); err != nil {
		return Tenant{}, err
	}
	row := c.db.QueryRowContext(ctx, "SELECT id,name,owner,template,source,status,host,created_at,updated_at FROM tenants "+clause, value)
	ten, err := c.scanTenant(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("get tenant: %w", err)
	}
	return ten, nil
}

type scanner interface{ Scan(...any) error }

func (c *Catalog) scanTenant(row scanner) (Tenant, error) {
	var tenant Tenant
	var host sql.NullString
	var created, updated int64
	if err := row.Scan(&tenant.ID, &tenant.Name, &tenant.Owner, &tenant.Template, &tenant.Source, &tenant.Status, &host, &created, &updated); err != nil {
		return Tenant{}, err
	}
	tenant.Host = host.String
	tenant.CreatedAt = time.Unix(0, created).UTC()
	tenant.UpdatedAt = time.Unix(0, updated).UTC()
	tenant.Paths = c.tenantPaths(tenant.ID)
	return tenant, nil
}

func (c *Catalog) tenantPaths(id string) Paths {
	root := filepath.Join(c.dataRoot, "tenants", id)
	return Paths{Root: root, Database: filepath.Join(root, "relay.db"), Blobs: filepath.Join(root, "blobs"), Git: filepath.Join(root, "git")}
}

func makeTenantDirs(paths Paths) error {
	for _, path := range []string{paths.Root, paths.Blobs, paths.Git} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func changedOrNotFound(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check catalog update: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func contextError(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

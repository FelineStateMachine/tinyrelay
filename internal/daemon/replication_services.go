package daemon

// This file contains the concrete adapters that keep replication tenant
// scoped. The replication package stays independent from daemon ownership,
// filesystem layout, and community implementation details.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/configport"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/records"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/work"
)

type tenantReplicationProvider struct{ tenant *Tenant }

func (t *Tenant) ReplicationBackupProvider() replication.BackupProvider {
	return tenantReplicationProvider{tenant: t}
}

func (p tenantReplicationProvider) Snapshot(ctx context.Context) (replication.BackupState, error) {
	if p.tenant == nil {
		return replication.BackupState{}, errors.New("daemon: nil tenant")
	}
	ctx, done, admissionErr := p.tenant.beginMaintenance(ctx)
	if admissionErr != nil {
		return replication.BackupState{}, admissionErr
	}
	defer done()
	state := replication.BackupState{}
	policy, err := json.Marshal(p.tenant.Policy())
	if err != nil {
		return state, err
	}
	state.Config = policy
	pubkey := ""
	if p.tenant.records != nil {
		pubkey = p.tenant.records.PublicKey()
	}
	identity, err := json.Marshal(map[string]string{"pubkey": pubkey, "relay": p.tenant.RelayURL()})
	if err != nil {
		return state, err
	}
	state.Identity = identity
	state.Database, err = snapshotTables(ctx, p.tenant.store)
	if err != nil {
		return state, err
	}
	state.Blobs, err = snapshotFiles(filepath.Join(p.tenant.meta.Paths.Root, "blobs"))
	if err != nil {
		return state, err
	}
	state.Git, err = snapshotFiles(p.tenant.meta.Paths.Git)
	if err != nil {
		return state, err
	}
	return state, nil
}

func (p tenantReplicationProvider) SnapshotArchive(ctx context.Context, now int64) (replication.BackupArchive, error) {
	state, err := p.Snapshot(ctx)
	if err != nil {
		return replication.BackupArchive{}, err
	}
	archive := replication.BackupArchive{Format: replication.BackupFormat, Created: now, State: state}
	for _, table := range state.Database {
		if table.Name != "events" {
			continue
		}
		for _, row := range table.Rows {
			for i, column := range table.Columns {
				if column != "raw" || i >= len(row) {
					continue
				}
				var raw string
				if err := json.Unmarshal(row[i], &raw); err != nil {
					return replication.BackupArchive{}, err
				}
				var item event.Event
				if err := json.Unmarshal([]byte(raw), &item); err != nil {
					return replication.BackupArchive{}, err
				}
				archive.Events = append(archive.Events, item)
			}
		}
	}
	return replication.SealBackup(archive)
}

func (p tenantReplicationProvider) Restore(ctx context.Context, state replication.BackupState) error {
	if p.tenant == nil {
		return errors.New("daemon: nil tenant")
	}
	ctx, done, admissionErr := p.tenant.beginMaintenance(ctx)
	if admissionErr != nil {
		return admissionErr
	}
	defer done()
	oldBlobs, err := snapshotFiles(filepath.Join(p.tenant.meta.Paths.Root, "blobs"))
	if err != nil {
		return err
	}
	oldGit, err := snapshotFiles(p.tenant.meta.Paths.Git)
	if err != nil {
		return err
	}
	oldTables, err := snapshotTables(ctx, p.tenant.store)
	if err != nil {
		return err
	}
	restored := false
	defer func() {
		if restored {
			return
		}
		// Restore is a single maintenance operation. Any failure after the
		// first write must leave both SQLite and tenant files as they were.
		_ = restoreTables(context.Background(), p.tenant.store, oldTables)
		_ = restoreFiles(filepath.Join(p.tenant.meta.Paths.Root, "blobs"), oldBlobs)
		_ = restoreFiles(p.tenant.meta.Paths.Git, oldGit)
		if p.tenant.git != nil {
			_ = p.tenant.git.Reload(context.WithoutCancel(ctx))
		}
	}()
	if err := restoreTables(ctx, p.tenant.store, state.Database); err != nil {
		return err
	}
	if err := restoreFiles(filepath.Join(p.tenant.meta.Paths.Root, "blobs"), state.Blobs); err != nil {
		_ = restoreTables(ctx, p.tenant.store, oldTables)
		_ = restoreFiles(filepath.Join(p.tenant.meta.Paths.Root, "blobs"), oldBlobs)
		return err
	}
	if err := restoreFiles(p.tenant.meta.Paths.Git, state.Git); err != nil {
		_ = restoreTables(ctx, p.tenant.store, oldTables)
		_ = restoreFiles(filepath.Join(p.tenant.meta.Paths.Root, "blobs"), oldBlobs)
		_ = restoreFiles(p.tenant.meta.Paths.Git, oldGit)
		return err
	}
	if len(state.GitSQL) > 0 && string(state.GitSQL) != "null" {
		if p.tenant.git == nil {
			_ = restoreTables(ctx, p.tenant.store, oldTables)
			_ = restoreFiles(filepath.Join(p.tenant.meta.Paths.Root, "blobs"), oldBlobs)
			_ = restoreFiles(filepath.Join(p.tenant.meta.Paths.Git), oldGit)
			return errors.New("daemon: Git backup migration unavailable")
		}
		if _, err := gitrelay.ValidateGitBackup(state.GitSQL); err != nil {
			_ = restoreTables(ctx, p.tenant.store, oldTables)
			_ = restoreFiles(filepath.Join(p.tenant.meta.Paths.Root, "blobs"), oldBlobs)
			_ = restoreFiles(filepath.Join(p.tenant.meta.Paths.Git), oldGit)
			return fmt.Errorf("daemon: validate legacy Git backup: %w", err)
		}
		if err := p.tenant.git.ImportGitBackup(ctx, state.GitSQL); err != nil {
			_ = restoreTables(ctx, p.tenant.store, oldTables)
			_ = restoreFiles(filepath.Join(p.tenant.meta.Paths.Root, "blobs"), oldBlobs)
			_ = restoreFiles(filepath.Join(p.tenant.meta.Paths.Git), oldGit)
			return fmt.Errorf("daemon: import legacy Git backup: %w", err)
		}
	}
	if len(state.Config) > 0 {
		var envelope struct {
			Format string `json:"format"`
		}
		if json.Unmarshal(state.Config, &envelope) == nil && strings.HasPrefix(envelope.Format, "bind.ws/relay-config/") {
			legacyConfig := true
			if p.tenant.config == nil {
				return errors.New("daemon: config importer unavailable for legacy backup")
			}
			parsed, err := configport.Parse(state.Config)
			if err != nil {
				return fmt.Errorf("daemon: legacy config: %w", err)
			}
			var identity struct {
				Owner string `json:"owner"`
			}
			if len(state.Identity) > 0 {
				if err := json.Unmarshal(state.Identity, &identity); err != nil {
					return fmt.Errorf("daemon: legacy identity: %w", err)
				}
			}
			if _, err := p.tenant.config.ApplyWithOptions(ctx, parsed, configport.ApplyOptions{MigrationOwner: identity.Owner}); err != nil {
				return fmt.Errorf("daemon: legacy config apply: %w", err)
			}
			fresh, err := records.New(ctx, records.Config{Store: p.tenant.store, Policy: p.tenant.Policy, RelayURL: p.tenant.RelayURL(), GroupID: p.tenant.meta.Name, OnGenerated: p.tenant.generatedRecord, DeliverNotification: p.tenant.deliverNotification, SetPolicy: func(next policy.Policy) error { return p.tenant.applyPolicy(context.Background(), next) }})
			if err != nil {
				return fmt.Errorf("daemon: refresh records after legacy config: %w", err)
			}
			if legacyConfig {
				p.tenant.records = fresh
			}
		} else {
			var next policy.Policy
			if err := json.Unmarshal(state.Config, &next); err != nil {
				return err
			}
			if next.Owner == "" {
				var envelope struct {
					Policy policy.Policy `json:"policy"`
				}
				if err := json.Unmarshal(state.Config, &envelope); err != nil {
					return err
				}
				next = envelope.Policy
				if next.Owner == "" {
					next.Owner = p.tenant.Policy().Owner
				}
			}
			p.tenant.community.SetOwner(next.Owner)
			if p.tenant.router != nil {
				p.tenant.replacePolicy(next)
			} else {
				p.tenant.mu.Lock()
				p.tenant.policy = next
				p.tenant.mu.Unlock()
			}
			fresh, err := records.New(ctx, records.Config{Store: p.tenant.store, Policy: p.tenant.Policy, RelayURL: p.tenant.RelayURL(), GroupID: p.tenant.meta.Name, OnGenerated: p.tenant.generatedRecord, DeliverNotification: p.tenant.deliverNotification, SetPolicy: func(next policy.Policy) error { return p.tenant.applyPolicy(context.Background(), next) }})
			if err != nil {
				return fmt.Errorf("daemon: refresh records identity: %w", err)
			}
			p.tenant.records = fresh
		}
	}
	if len(state.Blobs) > 0 {
		for _, object := range state.Blobs {
			if object.Type == "" {
				continue
			}
			if _, err := p.tenant.store.DB().ExecContext(ctx, `INSERT OR REPLACE INTO blobs(sha256,size,type,uploader,uploaded) VALUES(?,?,?,?,?)`, object.SHA256, len(object.Content), object.Type, object.Uploader, object.Uploaded); err != nil {
				return err
			}
		}
	}
	if len(state.Community) > 0 {
		if err := restoreRetainedState(ctx, p.tenant.store, state.Community); err != nil {
			return fmt.Errorf("daemon: restore retained state: %w", err)
		}
	}
	if p.tenant.git != nil {
		if err := p.tenant.git.Reload(ctx); err != nil {
			return fmt.Errorf("daemon: reload Git after restore: %w", err)
		}
	}
	restored = true
	return nil
}

func (p tenantReplicationProvider) FinalizeRestore(ctx context.Context) error {
	if p.tenant == nil {
		return errors.New("daemon: nil tenant")
	}
	if err := p.tenant.store.WithTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		for _, statement := range []string{
			`CREATE TABLE IF NOT EXISTS replication_pending_until(id TEXT PRIMARY KEY NOT NULL, until INTEGER NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS replication_pr_refs(repo TEXT NOT NULL, ref TEXT NOT NULL, until INTEGER NOT NULL, PRIMARY KEY(repo,ref))`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_events WHERE id IN (SELECT id FROM replication_pending_until WHERE until<=?)`, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM replication_pending_until WHERE until<=?`, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM replication_pr_refs WHERE until>0 AND until<=?`, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO pending_events(id,reason) SELECT p.id,'backup' FROM replication_pending_until p JOIN events e ON e.id=p.id WHERE p.until>?`, now)
		return err
	}); err != nil {
		return err
	}
	if p.tenant.git != nil {
		return p.tenant.git.Reload(ctx)
	}
	return nil
}

// restoreRetainedState keeps the bind.ws relay's auxiliary state even when
// the current schema has no first-class feature for one of those records. The
// small native tables are deliberately explicit: a restore must either write
// every record or return an error, never silently discard a family.
func restoreRetainedState(ctx context.Context, store *storage.Store, rawState []byte) error {
	var retained struct {
		ListHistory []string `json:"listHistory"`
		Hidden      []string `json:"hidden"`
		Hosted      []string `json:"hosted"`
		Pending     []struct {
			ID    string `json:"id"`
			Until int64  `json:"until"`
		} `json:"pending"`
		PRRefs []struct {
			Repo  string `json:"repo"`
			Ref   string `json:"ref"`
			Until int64  `json:"until"`
		} `json:"prRefs"`
	}
	if err := json.Unmarshal(rawState, &retained); err != nil {
		return fmt.Errorf("invalid state: %w", err)
	}
	now := time.Now().Unix()
	return store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS replication_hosted_events(id TEXT PRIMARY KEY NOT NULL)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS replication_pending_until(id TEXT PRIMARY KEY NOT NULL, until INTEGER NOT NULL)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS replication_pr_refs(repo TEXT NOT NULL, ref TEXT NOT NULL, until INTEGER NOT NULL, PRIMARY KEY(repo,ref))`); err != nil {
			return err
		}
		for _, encoded := range retained.ListHistory {
			var item event.Event
			if err := json.Unmarshal([]byte(encoded), &item); err != nil {
				return fmt.Errorf("list history event: %w", err)
			}
			if err := event.Validate(item); err != nil {
				return fmt.Errorf("list history event validation: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO list_history(owner,kind,d,event_id,created_at,saved_at,expires,raw) VALUES(?,?,?,?,?,?,?,?)`, item.PubKey, item.Kind, event.Tag(item, "d"), item.ID, item.CreatedAt, now, event.Expiration(item), encoded); err != nil {
				return err
			}
		}
		for _, id := range retained.Hidden {
			if id == "" {
				return errors.New("hidden event id is empty")
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO hidden_events(id,reason) VALUES(?,?)`, id, "backup"); err != nil {
				return err
			}
		}
		for _, id := range retained.Hosted {
			if id == "" {
				return errors.New("hosted event id is empty")
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO replication_hosted_events(id) VALUES(?)`, id); err != nil {
				return err
			}
		}
		for _, row := range retained.Pending {
			if row.ID == "" || row.Until < 0 {
				return errors.New("invalid pending event state")
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO replication_pending_until(id,until) VALUES(?,?)`, row.ID, row.Until); err != nil {
				return err
			}
			// pending_events references events, so retain the marker only when the
			// event is already present. The durable until table above preserves it
			// for the subsequent event import.
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO pending_events(id,reason) SELECT id,'backup' FROM events WHERE id=?`, row.ID); err != nil {
				return err
			}
		}
		for _, row := range retained.PRRefs {
			if row.Repo == "" || row.Ref == "" || row.Until < 0 {
				return errors.New("invalid pull request reference state")
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO replication_pr_refs(repo,ref,until) VALUES(?,?,?)`, row.Repo, row.Ref, row.Until); err != nil {
				return err
			}
		}
		return nil
	})
}

func snapshotTables(ctx context.Context, store *storage.Store) ([]replication.BackupTable, error) {
	var out []replication.BackupTable
	err := store.WithTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT name,sql FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name, definition string
			if err := rows.Scan(&name, &definition); err != nil {
				return err
			}
			if excludedSnapshotTable(name, definition) {
				continue
			}
			table, err := snapshotTableTx(ctx, tx, name)
			if err != nil {
				return err
			}
			out = append(out, table)
		}
		return rows.Err()
	})
	return out, err
}

func excludedSnapshotTable(name, definition string) bool {
	lower := strings.ToLower(name + " " + definition)
	for _, part := range []string{"work_intents", "replication_job_cursors", "browser_sessions", "auth_challenges", "push_queue", "push_delivered", "delivery_queue"} {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return strings.Contains(lower, "virtual table") || strings.HasSuffix(name, "_content") || strings.HasSuffix(name, "_data") || strings.HasSuffix(name, "_idx") || strings.HasSuffix(name, "_docsize") || strings.HasSuffix(name, "_config")
}

func snapshotTable(ctx context.Context, db *sql.DB, name string) (replication.BackupTable, error) {
	return snapshotTableDB(ctx, db, name)
}

func snapshotTableTx(ctx context.Context, tx *sql.Tx, name string) (replication.BackupTable, error) {
	if !safeIdentifier(name) {
		return replication.BackupTable{}, errors.New("daemon: unsafe table name")
	}
	rows, err := tx.QueryContext(ctx, `SELECT * FROM "`+name+`"`)
	if err != nil {
		return replication.BackupTable{}, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return replication.BackupTable{}, err
	}
	table := replication.BackupTable{Name: name, Columns: columns}
	for rows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := rows.Scan(destinations...); err != nil {
			return table, err
		}
		encoded := make([]json.RawMessage, len(values))
		for i, value := range values {
			raw, err := marshalDBValue(value)
			if err != nil {
				return table, err
			}
			encoded[i] = raw
		}
		table.Rows = append(table.Rows, encoded)
	}
	return table, rows.Err()
}

func snapshotTableDB(ctx context.Context, db *sql.DB, name string) (replication.BackupTable, error) {
	if !safeIdentifier(name) {
		return replication.BackupTable{}, errors.New("daemon: unsafe table name")
	}
	rows, err := db.QueryContext(ctx, "SELECT * FROM \""+name+"\"")
	if err != nil {
		return replication.BackupTable{}, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return replication.BackupTable{}, err
	}
	out := replication.BackupTable{Name: name, Columns: cols}
	for rows.Next() {
		values := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return out, err
		}
		encoded := make([]json.RawMessage, len(values))
		for i, value := range values {
			raw, err := json.Marshal(value)
			if err != nil {
				return out, err
			}
			encoded[i] = raw
		}
		out.Rows = append(out.Rows, encoded)
	}
	return out, rows.Err()
}

func restoreTables(ctx context.Context, store *storage.Store, tables []replication.BackupTable) error {
	return store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys=ON`); err != nil {
			return err
		}
		for _, table := range tables {
			if !safeIdentifier(table.Name) {
				return errors.New("daemon: unsafe table name")
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM \""+table.Name+"\""); err != nil {
				return err
			}
			for _, row := range table.Rows {
				if len(row) != len(table.Columns) {
					return errors.New("daemon: malformed table row")
				}
				columns := make([]string, len(table.Columns))
				marks := make([]string, len(table.Columns))
				args := make([]any, len(row))
				for i, column := range table.Columns {
					if !safeIdentifier(column) {
						return errors.New("daemon: unsafe column name")
					}
					columns[i] = "\"" + column + "\""
					marks[i] = "?"
					value, err := unmarshalDBValue(row[i])
					if err != nil {
						return err
					}
					args[i] = value
				}
				query := "INSERT INTO \"" + table.Name + "\" (" + strings.Join(columns, ",") + ") VALUES (" + strings.Join(marks, ",") + ")"
				if _, err := tx.ExecContext(ctx, query, args...); err != nil {
					return fmt.Errorf("restore %s: %w", table.Name, err)
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM search`); err == nil {
			if _, err := tx.ExecContext(ctx, `INSERT INTO search(rowid,content) SELECT seq,json_extract(raw,'$.content') FROM events WHERE json_extract(raw,'$.content')<>''`); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE replication_jobs SET state='pending',phase='pending',error='',stored=0,sent=0,skipped=0,refused=0,cursor=0,rounds=0,started_at=0,finished_at=0,updated_at=strftime('%s','now')`); err != nil {
			// Older tenant databases may not have the replication job table yet.
			if !strings.Contains(err.Error(), "no such table") {
				return err
			}
		}
		return nil
	})
}

func marshalDBValue(value any) (json.RawMessage, error) {
	if bytesValue, ok := value.([]byte); ok {
		return json.Marshal(map[string]string{"$bytes": base64.StdEncoding.EncodeToString(bytesValue)})
	}
	return json.Marshal(value)
}

func unmarshalDBValue(raw json.RawMessage) (any, error) {
	var marker map[string]string
	if err := json.Unmarshal(raw, &marker); err == nil && marker["$bytes"] != "" {
		return base64.StdEncoding.DecodeString(marker["$bytes"])
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if number, ok := value.(json.Number); ok {
		if integer, err := strconv.ParseInt(string(number), 10, 64); err == nil {
			return integer, nil
		}
		return strconv.ParseFloat(string(number), 64)
	}
	return value, nil
}

func snapshotFiles(root string) ([]replication.BackupObject, error) {
	if root == "" {
		return nil, nil
	}
	var out []replication.BackupObject
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, replication.BackupObject{Name: rel, SHA256: hex.EncodeToString(sum[:]), Content: body})
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return out, err
}
func restoreFiles(root string, objects []replication.BackupObject) error {
	allowed := make(map[string]struct{}, len(objects))
	for _, object := range objects {
		allowed[filepath.Clean(object.Name)] = struct{}{}
	}
	if err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if info.IsDir() || path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if _, ok := allowed[filepath.Clean(rel)]; !ok {
			return os.Remove(path)
		}
		return nil
	}); err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, object := range objects {
		if object.Name == "" || filepath.IsAbs(object.Name) || strings.HasPrefix(filepath.Clean(object.Name), ".."+string(filepath.Separator)) {
			return errors.New("daemon: unsafe backup file")
		}
		sum := sha256.Sum256(object.Content)
		if hex.EncodeToString(sum[:]) != object.SHA256 {
			return errors.New("daemon: backup file checksum mismatch")
		}
		path := filepath.Join(root, object.Name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(path, object.Content, 0600); err != nil {
			return err
		}
	}
	return nil
}
func safeIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

type tenantDiscovery struct {
	tenant    *Tenant
	transport *replication.NostrTransport
	seeds     []string
}

func (t *Tenant) ReplicationDiscovery(seeds []string) replication.Discovery {
	dialer := replication.WebsocketDialer{}
	if t != nil && t.app != nil {
		dialer.AllowPrivate = t.app.cfg.AllowPrivateRelays
		dialer.MaxMessageBytes = t.app.cfg.MaxMessageBytes
	}
	return tenantDiscovery{tenant: t, transport: &replication.NostrTransport{Dialer: dialer}, seeds: append([]string(nil), seeds...)}
}
func (d tenantDiscovery) DiscoverRelays(ctx context.Context, pubkey string) ([]string, error) {
	return d.discover(ctx, pubkey, false)
}

func (d tenantDiscovery) DiscoverReadRelays(ctx context.Context, pubkey string) ([]string, error) {
	return d.discover(ctx, pubkey, true)
}

func (d tenantDiscovery) discover(ctx context.Context, pubkey string, read bool) ([]string, error) {
	if d.tenant == nil {
		return nil, errors.New("daemon: nil tenant")
	}
	directory := localDirectory{store: d.tenant.store}
	local := directory.WriteRelays(pubkey)
	if read {
		local = directory.ReadRelays(pubkey)
	}
	if len(local) > 0 {
		return local, nil
	}
	if d.transport == nil || len(d.seeds) == 0 {
		return nil, nil
	}
	for _, seed := range d.seeds {
		events, err := d.transport.Query(ctx, seed, event.Filter{Authors: []string{pubkey}, Kinds: []int{10002}, Tags: map[string][]string{}, Limit: intPtr(1)})
		if err != nil {
			continue
		}
		for _, item := range events {
			if err := event.Validate(item); err != nil {
				continue
			}
			if _, err := d.tenant.store.Save(ctx, item, storage.SaveOptions{Now: time.Now().Unix()}); err != nil && err != storage.ErrDuplicate && err != storage.ErrReplaced {
				continue
			}
			stored := localDirectory{store: d.tenant.store}
			if read {
				return stored.ReadRelays(pubkey), nil
			}
			return stored.WriteRelays(pubkey), nil
		}
	}
	return nil, nil
}
func intPtr(value int) *int { return &value }

func (t *Tenant) PrepareReplicationCallbacks(ctx context.Context, e event.Event) ([]storage.Intent, error) {
	if t == nil || !t.Policy().Features.Push {
		return nil, nil
	}
	approved := t.callbackPolicy()
	rows, err := t.store.DB().QueryContext(ctx, `SELECT raw FROM events WHERE kind=? ORDER BY created_at DESC`, event.KIND_PUSH_REGISTRATION)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	registrations := make([]replication.PushRegistration, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var registration event.Event
		if err := json.Unmarshal([]byte(raw), &registration); err != nil {
			continue
		}
		parsed, parseErr := replication.ParsePushRegistration(registration, approved, t.RelayURL())
		if parseErr == nil {
			registrations = append(registrations, parsed)
		}
	}
	return replication.PrepareCallbackIntents(ctx, e, registrations), rows.Err()
}

// callbackPolicy keeps host approval separate from the tenant owner's
// registration list. A tenant can opt in an origin only after the operator has
// placed it in PushCallbackOrigins as well.
func (t *Tenant) callbackPolicy() replication.CallbackPolicy {
	p := t.Policy()
	return replication.CallbackPolicy{HostOrigins: append([]string(nil), t.app.cfg.PushCallbackOrigins...), OwnerOrigins: append([]string(nil), p.PushCallbacks...)}
}

// validatePushRegistration applies the source relay's admission rules before
// the registration is persisted. Callback workers revalidate later, but an
// invalid registration must never become durable state.
func (t *Tenant) validatePushRegistration(ctx context.Context, e event.Event, s relay.Session) error {
	if e.Kind != event.KIND_PUSH_REGISTRATION {
		return nil
	}
	if !t.Policy().Features.Push {
		return errors.New("unsupported: relay push is switched off")
	}
	if !containsString(s.PubKeys, e.PubKey) {
		return errors.New("auth-required: push registration must be authenticated as its author")
	}
	if role, err := t.community.Role(ctx, e.PubKey); err != nil {
		return err
	} else if role == "" {
		return errors.New("restricted: push registrations are limited to relay members")
	}
	if len(event.TagValues(e, "d")) != 1 || event.Tag(e, "d") == "" || len(event.Tag(e, "d")) > 256 {
		return errors.New("invalid: push registration needs one d tag")
	}
	if len(event.TagValues(e, "relay")) == 0 || len(event.TagValues(e, "relay")) > 4 || len(event.TagValues(e, "callback")) != 1 {
		return errors.New("invalid: push registration needs relay and callback tags")
	}
	for _, name := range []string{"filter", "ignore"} {
		values := event.TagValues(e, name)
		for _, raw := range values {
			var object map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &object); err != nil {
				return errors.New("invalid: push filter is not JSON")
			}
			if object == nil {
				return errors.New("invalid: push filter must be an object")
			}
			for key := range object {
				if !pushFilterField(key) {
					return errors.New("invalid: unknown push filter field")
				}
			}
		}
	}
	parsed, err := replication.ParsePushRegistration(e, t.callbackPolicy(), t.RelayURL())
	if err != nil {
		return err
	}
	for _, filter := range append(parsed.Filters, parsed.Ignore...) {
		for _, kind := range filter.Kinds {
			if kind < 0 || kind > 65535 {
				return errors.New("invalid: push filter value out of range")
			}
		}
		if filter.Since != nil && *filter.Since < 0 || filter.Until != nil && *filter.Until < 0 {
			return errors.New("invalid: push filter value out of range")
		}
	}
	return nil
}

func pushFilterField(key string) bool {
	if key == "ids" || key == "authors" || key == "kinds" || key == "since" || key == "until" {
		return true
	}
	return len(key) == 2 && key[0] == '#'
}

func (t *Tenant) ReplicationCallbackHandler() work.Handler {
	return replication.NewCallbackHandlerWithEventVisibility(t.store, replication.CallbackClient{}, func() replication.CallbackPolicy {
		return t.callbackPolicy()
	}, func() bool { return t.Policy().Features.Push }, func(ctx context.Context, pubkey string) bool {
		return t.Policy().Reads == "open" || func() bool { role, err := t.community.Role(ctx, pubkey); return err == nil && role != "" }()
	}, func(ctx context.Context, pubkey string, delivered event.Event) bool {
		return t.gate.CanSee(ctx, delivered, relay.Session{PubKeys: []string{pubkey}, RelayURL: t.RelayURL()}, nil)
	})
}

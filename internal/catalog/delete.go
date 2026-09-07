package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Delete permanently removes a disabled tenant. The deleting state and intent
// make the operation safe to resume after a process or host failure.
func (c *Catalog) Delete(ctx context.Context, id string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	root := c.tenantPaths(id).Root
	staging := filepath.Join(filepath.Dir(root), ".deleting-"+id)
	if err := c.beginDeletion(ctx, id, root, staging); err != nil {
		return err
	}
	if err := stageTenant(root, staging); err != nil {
		return err
	}
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("remove tenant data: %w", err)
	}
	return c.finishDeletion(ctx, id)
}

func (c *Catalog) beginDeletion(ctx context.Context, id, root, staging string) error {
	return c.dbTransaction(ctx, func(tx *sql.Tx) error {
		var status Status
		if err := tx.QueryRowContext(ctx, "SELECT status FROM tenants WHERE id=?", id).Scan(&status); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		} else if status != StatusDisabled {
			return ErrInvalidTransition
		}
		now := time.Now().UTC().UnixNano()
		if _, err := tx.ExecContext(ctx, "UPDATE tenants SET status=?,updated_at=? WHERE id=? AND status=?", StatusDeleting, now, id, StatusDisabled); err != nil {
			return fmt.Errorf("mark tenant deleting: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO tenant_deletions(id,root,staging,started_at) VALUES(?,?,?,?)", id, root, staging, now); err != nil {
			return fmt.Errorf("record tenant deletion: %w", err)
		}
		return nil
	})
}

func (c *Catalog) finishDeletion(ctx context.Context, id string) error {
	return c.dbTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM tenant_hosts WHERE tenant_id=?", id); err != nil {
			return fmt.Errorf("delete tenant hosts: %w", err)
		}
		result, err := tx.ExecContext(ctx, "DELETE FROM tenants WHERE id=? AND status=?", id, StatusDeleting)
		if err != nil {
			return fmt.Errorf("delete tenant catalog row: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM tenant_deletions WHERE id=?", id)
		return err
	})
}

func (c *Catalog) recoverDeletions(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, "SELECT id,root,staging FROM tenant_deletions ORDER BY started_at,id")
	if err != nil {
		return fmt.Errorf("scan tenant deletions: %w", err)
	}
	defer rows.Close()
	type intent struct{ id, root, staging string }
	var intents []intent
	for rows.Next() {
		var item intent
		if err := rows.Scan(&item.id, &item.root, &item.staging); err != nil {
			return err
		}
		intents = append(intents, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range intents {
		if err := stageTenant(item.root, item.staging); err != nil {
			return fmt.Errorf("recover tenant %q: %w", item.id, err)
		}
		if err := os.RemoveAll(item.staging); err != nil {
			return fmt.Errorf("recover tenant data %q: %w", item.id, err)
		}
		if err := c.finishDeletion(ctx, item.id); err != nil {
			return fmt.Errorf("recover tenant catalog %q: %w", item.id, err)
		}
	}
	return nil
}

func stageTenant(root, staging string) error {
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		if _, stagingErr := os.Stat(staging); stagingErr == nil || errors.Is(stagingErr, os.ErrNotExist) {
			return nil
		} else {
			return stagingErr
		}
	} else if err != nil {
		return err
	}
	return os.Rename(root, staging)
}

func (c *Catalog) dbTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

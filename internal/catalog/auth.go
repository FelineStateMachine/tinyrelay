package catalog

import (
	"context"
	"database/sql"
	"time"
)

// ConsumeAuth atomically records a signed request identifier. It returns
// false when the identifier has already been consumed, providing replay
// protection for process-level creation endpoints across restarts.
func (c *Catalog) ConsumeAuth(ctx context.Context, id string, expires time.Time) (bool, error) {
	if id == "" {
		return false, nil
	}
	if _, err := c.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS auth_nonces(id TEXT PRIMARY KEY, expires_at INTEGER NOT NULL)`); err != nil {
		return false, err
	}
	if _, err := c.db.ExecContext(ctx, `DELETE FROM auth_nonces WHERE expires_at < ?`, time.Now().Unix()); err != nil {
		return false, err
	}
	var inserted int64
	err := c.db.QueryRowContext(ctx, `INSERT INTO auth_nonces(id,expires_at) VALUES(?,?) ON CONFLICT(id) DO NOTHING RETURNING 1`, id, expires.Unix()).Scan(&inserted)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil && inserted == 1, err
}

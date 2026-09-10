package sites

import (
	"context"
	"database/sql"
)

const manifestSchema = `CREATE TABLE IF NOT EXISTS site_manifests (
	label TEXT PRIMARY KEY, event_id TEXT NOT NULL, kind INTEGER NOT NULL,
	pubkey TEXT NOT NULL, d TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL
);
CREATE TRIGGER IF NOT EXISTS site_manifest_event_deleted
AFTER DELETE ON events
WHEN old.kind IN (5128,15128,35128)
BEGIN
	DELETE FROM site_manifests WHERE event_id=old.id;
END;`

func ensureManifestSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, manifestSchema)
	return err
}

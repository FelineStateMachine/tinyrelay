package replication

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const jobSchema = `CREATE TABLE IF NOT EXISTS replication_jobs (id TEXT PRIMARY KEY, payload TEXT NOT NULL, state TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, phase TEXT NOT NULL DEFAULT 'pending', error TEXT NOT NULL DEFAULT '', stored INTEGER NOT NULL DEFAULT 0, sent INTEGER NOT NULL DEFAULT 0, skipped INTEGER NOT NULL DEFAULT 0, refused INTEGER NOT NULL DEFAULT 0, cursor INTEGER NOT NULL DEFAULT 0, rounds INTEGER NOT NULL DEFAULT 0, started_at INTEGER NOT NULL DEFAULT 0, finished_at INTEGER NOT NULL DEFAULT 0); CREATE TABLE IF NOT EXISTS replication_job_cursors (job_id TEXT NOT NULL, target TEXT NOT NULL, cursor INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(job_id,target))`

// EnsureJobSchema initializes replication-owned job state for callers that
// import configuration before constructing a replication service.
func EnsureJobSchema(db interface {
	Exec(string, ...any) (sql.Result, error)
}) error {
	_, err := db.Exec(jobSchema)
	return err
}

type ConfigJob struct {
	ID      string
	Payload []byte
}

// ReplaceJobsTx replaces the durable replication jobs represented by specs in
// a caller-owned transaction. It is used by configuration import so policy,
// config metadata and replication state commit atomically.
func ReplaceJobsTx(ctx context.Context, tx *sql.Tx, specs []ConfigJob) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM replication_jobs`); err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, spec := range specs {
		if spec.ID == "" {
			return fmt.Errorf("replication: config job id is required")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO replication_jobs(id,payload,state,created_at,updated_at) VALUES(?,?, 'pending',?,?)`, spec.ID, string(spec.Payload), now, now); err != nil {
			return err
		}
	}
	return nil
}

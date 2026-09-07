package daemon

import (
	"context"
	"database/sql"
	"strings"
)

// sweepReplicationState mirrors bind.ws pending-state expiry. Pending Git
// events are removed when their retained deadline passes; promotion removes
// the deadline before this sweep can run.
func (t *Tenant) sweepReplicationState(ctx context.Context, now int64) error {
	var pending []string
	rows, err := t.store.DB().QueryContext(ctx, `SELECT id FROM replication_pending_until WHERE until>0 AND until<=?`, now)
	if err == nil {
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			pending = append(pending, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	if t.git != nil && len(pending) > 0 {
		if err := t.git.ExpirePending(ctx, pending); err != nil {
			return err
		}
	}
	if t.git != nil {
		rows, err := t.store.DB().QueryContext(ctx, `SELECT repo,ref FROM replication_pr_refs WHERE until>0 AND until<=?`, now)
		if err == nil {
			for rows.Next() {
				var repo, ref string
				if err := rows.Scan(&repo, &ref); err != nil {
					rows.Close()
					return err
				}
				var pruneErr error
				if strings.HasPrefix(repo, "pr:") || strings.HasPrefix(repo, "30617:") {
					pruneErr = t.git.DeletePRRef(ctx, repo, ref)
				} else {
					pruneErr = t.git.PruneRef(ctx, repo, ref)
				}
				if pruneErr != nil {
					rows.Close()
					return pruneErr
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
		}
	}
	return t.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE id IN (SELECT id FROM replication_pending_until WHERE until>0 AND until<=?)`, now); err != nil && !isMissingTable(err) {
			return err
		}
		for _, query := range []string{
			`DELETE FROM pending_events WHERE id IN (SELECT id FROM replication_pending_until WHERE until>0 AND until<=?)`,
			`DELETE FROM replication_pending_until WHERE until>0 AND until<=?`,
			`DELETE FROM replication_pr_refs WHERE until>0 AND until<=?`,
		} {
			if _, err := tx.ExecContext(ctx, query, now); err != nil && !isMissingTable(err) {
				return err
			}
		}
		return nil
	})
}

func isMissingTable(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "no such table") || strings.Contains(err.Error(), "no such column"))
}

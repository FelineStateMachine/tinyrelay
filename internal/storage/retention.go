package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

type RetentionRule struct {
	Kind int
	Days int
}
type MemberRetention struct {
	PubKey string
	Days   int
}
type RetentionOptions struct {
	Now      int64
	Identity string
	Rules    []RetentionRule
	Members  []MemberRetention
}

// SweepRetention enforces user-selected content retention, independent of host
// capacity. The catch-all deliberately retains replaceable lists and mail.
func (s *Store) SweepRetention(ctx context.Context, opts RetentionOptions) (int64, error) {
	var removed int64
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		specific := []int{}
		for _, rule := range opts.Rules {
			if rule.Kind >= 0 {
				specific = append(specific, rule.Kind)
			}
		}
		encoded, err := json.Marshal(specific)
		if err != nil {
			return err
		}
		for _, rule := range opts.Rules {
			if rule.Days <= 0 {
				continue
			}
			q := "DELETE FROM events WHERE created_at<? AND pubkey<>?"
			args := []any{opts.Now - int64(rule.Days)*86400, opts.Identity}
			if rule.Kind < 0 {
				q += " AND " + keepableKinds + " AND kind<>1059 AND kind NOT IN (SELECT value FROM json_each(?))"
				args = append(args, string(encoded))
			} else {
				q += " AND kind=?"
				args = append(args, rule.Kind)
			}
			n, err := deleteCount(ctx, tx, q, args...)
			if err != nil {
				return err
			}
			removed += n
		}
		for _, member := range opts.Members {
			if member.Days <= 0 {
				continue
			}
			before := opts.Now - int64(member.Days)*86400
			n, err := deleteCount(ctx, tx, "DELETE FROM events WHERE pubkey=? AND created_at<? AND "+keepableKinds, member.PubKey, before)
			if err != nil {
				return err
			}
			removed += n
			if _, err := tx.ExecContext(ctx, "DELETE FROM list_history WHERE owner=? AND created_at<?", member.PubKey, before); err != nil {
				return err
			}
		}
		return nil
	})
	return removed, err
}

const keepableKinds = `kind NOT IN(0,3,10002,9735,13534,8000,8001,9000,9001,33534,39000,39001,39002,39003,39005)
 AND kind NOT BETWEEN 10000 AND 19999 AND kind NOT BETWEEN 30000 AND 39999`

func deleteCount(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("sweep retention: %w", err)
	}
	return result.RowsAffected()
}

type KindStats struct {
	Kind   int   `json:"kind"`
	N      int64 `json:"n"`
	Bytes  int64 `json:"bytes"`
	Oldest int64 `json:"oldest"`
	Newest int64 `json:"newest"`
}

func (s *Store) KindStats(ctx context.Context) ([]KindStats, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT kind,count(*),sum(length(raw)),min(created_at),max(created_at) FROM events WHERE kind<>30390 GROUP BY kind ORDER BY sum(length(raw)) DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []KindStats{}
	for rows.Next() {
		var row KindStats
		if err := rows.Scan(&row.Kind, &row.N, &row.Bytes, &row.Oldest, &row.Newest); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

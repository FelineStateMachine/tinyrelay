package community

import (
	"context"
	"database/sql"
	"time"
)

// ConfigSnapshot is the community-owned portion of relay configuration.
// The type deliberately contains no configport types so the domain owns its
// storage contract.
type ConfigSnapshot struct {
	Members   []Member
	Bans      []ConfigBan
	IPBlocks  []ConfigIPBlock
	EventBans []ConfigEventBan
	KindRules []ConfigKindRule
	Retention []ConfigRetention
}

type ConfigBan struct {
	PubKey string
	Reason string
}

type ConfigIPBlock struct {
	IP     string
	Reason string
}

type ConfigEventBan struct {
	ID     string
	Reason string
}

type ConfigKindRule struct {
	Kind int
	Rule string
}

type ConfigRetention struct {
	Kind *int
	Days int
}

type ConfigMemberRetention struct {
	PubKey string
	Days   int
}

// RetentionRules returns the community-owned retention settings.
func (s *Service) RetentionRules(ctx context.Context) ([]ConfigRetention, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT kind,days FROM community_retention ORDER BY kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConfigRetention
	for rows.Next() {
		var kind, days int
		if err := rows.Scan(&kind, &days); err != nil {
			return nil, err
		}
		out = append(out, ConfigRetention{Kind: &kind, Days: days})
	}
	return out, rows.Err()
}

// MembersForRetention returns members with an explicit retention override.
func (s *Service) MembersForRetention(ctx context.Context) ([]ConfigMemberRetention, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT pubkey,keep_days FROM community_members WHERE keep_days>0 ORDER BY pubkey`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConfigMemberRetention
	for rows.Next() {
		var member ConfigMemberRetention
		if err := rows.Scan(&member.PubKey, &member.Days); err != nil {
			return nil, err
		}
		out = append(out, member)
	}
	return out, rows.Err()
}

// ExportConfig reads the community-owned configuration tables.
func ExportConfig(ctx context.Context, store interface {
	DB() *sql.DB
}) (ConfigSnapshot, error) {
	var out ConfigSnapshot
	rows, err := store.DB().QueryContext(ctx, `SELECT pubkey,name,note,role,invited_by,via,created_at,keep_days FROM community_members WHERE role<>'owner' ORDER BY pubkey`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var member Member
		if err := rows.Scan(&member.PubKey, &member.Name, &member.Note, &member.Role, &member.InvitedBy, &member.Via, &member.CreatedAt, &member.KeepDays); err != nil {
			return out, err
		}
		out.Members = append(out.Members, member)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if err := readConfigRows(ctx, store.DB(), `SELECT pubkey,reason FROM community_bans ORDER BY pubkey`, func(rows *sql.Rows) error {
		var item ConfigBan
		if err := rows.Scan(&item.PubKey, &item.Reason); err != nil {
			return err
		}
		out.Bans = append(out.Bans, item)
		return nil
	}); err != nil {
		return out, err
	}
	if err := readConfigRows(ctx, store.DB(), `SELECT ip,reason FROM community_ip_blocks ORDER BY ip`, func(rows *sql.Rows) error {
		var item ConfigIPBlock
		if err := rows.Scan(&item.IP, &item.Reason); err != nil {
			return err
		}
		out.IPBlocks = append(out.IPBlocks, item)
		return nil
	}); err != nil {
		return out, err
	}
	if err := readConfigRows(ctx, store.DB(), `SELECT id,reason FROM community_event_bans ORDER BY id`, func(rows *sql.Rows) error {
		var item ConfigEventBan
		if err := rows.Scan(&item.ID, &item.Reason); err != nil {
			return err
		}
		out.EventBans = append(out.EventBans, item)
		return nil
	}); err != nil {
		return out, err
	}
	if err := readConfigRows(ctx, store.DB(), `SELECT kind,rule FROM community_kind_rules ORDER BY kind`, func(rows *sql.Rows) error {
		var item ConfigKindRule
		if err := rows.Scan(&item.Kind, &item.Rule); err != nil {
			return err
		}
		out.KindRules = append(out.KindRules, item)
		return nil
	}); err != nil {
		return out, err
	}
	if err := readConfigRows(ctx, store.DB(), `SELECT kind,days FROM community_retention ORDER BY kind`, func(rows *sql.Rows) error {
		var kind, days int
		if err := rows.Scan(&kind, &days); err != nil {
			return err
		}
		if kind < 0 {
			out.Retention = append(out.Retention, ConfigRetention{Days: days})
			return nil
		}
		out.Retention = append(out.Retention, ConfigRetention{Kind: &kind, Days: days})
		return nil
	}); err != nil {
		return out, err
	}
	return out, nil
}

func readConfigRows(ctx context.Context, db *sql.DB, query string, scan func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ApplyConfigTx replaces each selected community configuration section inside
// the caller-owned transaction. A selected empty section clears that section;
// an unselected section is left unchanged. The caller commits or rolls back
// the transaction after all related configuration writes succeed.
func ApplyConfigTx(ctx context.Context, tx *sql.Tx, c ConfigSnapshot, members, bans, addresses, eventBans, kinds, retention bool) error {
	now := time.Now().Unix()
	if members {
		if _, err := tx.ExecContext(ctx, `DELETE FROM community_members WHERE role<>'owner'`); err != nil {
			return err
		}
		for _, member := range c.Members {
			if _, err := tx.ExecContext(ctx, `INSERT INTO community_members(pubkey,name,note,role,invited_by,via,created_at,keep_days) VALUES(?,?,?,?,?,?,?,?)`, member.PubKey, member.Name, member.Note, member.Role, member.InvitedBy, member.Via, now, member.KeepDays); err != nil {
				return err
			}
		}
	}
	if bans {
		if _, err := tx.ExecContext(ctx, `DELETE FROM community_bans`); err != nil {
			return err
		}
		for _, item := range c.Bans {
			if item.PubKey != "" {
				if _, err := tx.ExecContext(ctx, `INSERT INTO community_bans(pubkey,reason,at) VALUES(?,?,?)`, item.PubKey, item.Reason, now); err != nil {
					return err
				}
			}
		}
	}
	if addresses {
		if _, err := tx.ExecContext(ctx, `DELETE FROM community_ip_blocks`); err != nil {
			return err
		}
		for _, item := range c.IPBlocks {
			if item.IP != "" {
				if _, err := tx.ExecContext(ctx, `INSERT INTO community_ip_blocks(ip,reason,at) VALUES(?,?,?)`, item.IP, item.Reason, now); err != nil {
					return err
				}
			}
		}
	}
	if eventBans {
		if _, err := tx.ExecContext(ctx, `DELETE FROM community_event_bans`); err != nil {
			return err
		}
		for _, item := range c.EventBans {
			if item.ID != "" {
				if _, err := tx.ExecContext(ctx, `INSERT INTO community_event_bans(id,reason,at) VALUES(?,?,?)`, item.ID, item.Reason, now); err != nil {
					return err
				}
			}
		}
	}
	if kinds {
		if _, err := tx.ExecContext(ctx, `DELETE FROM community_kind_rules`); err != nil {
			return err
		}
		for _, item := range c.KindRules {
			if _, err := tx.ExecContext(ctx, `INSERT INTO community_kind_rules(kind,rule) VALUES(?,?)`, item.Kind, item.Rule); err != nil {
				return err
			}
		}
	}
	if retention {
		if _, err := tx.ExecContext(ctx, `DELETE FROM community_retention`); err != nil {
			return err
		}
		for _, item := range c.Retention {
			kind := -1
			if item.Kind != nil {
				kind = *item.Kind
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO community_retention(kind,days) VALUES(?,?)`, kind, item.Days); err != nil {
				return err
			}
		}
	}
	return nil
}

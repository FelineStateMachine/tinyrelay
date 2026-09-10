package community

import (
	"context"
	"errors"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/communityread"
)

// Members exposes the community-owned membership projection to records. The
// records package consumes this narrow read model and never relies on table
// names or columns.
func (s *Service) Members(ctx context.Context) ([]communityread.Member, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT pubkey,name,role FROM community_members ORDER BY pubkey`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []communityread.Member
	for rows.Next() {
		var member communityread.Member
		if err := rows.Scan(&member.PubKey, &member.Name, &member.Role); err != nil {
			return nil, err
		}
		out = append(out, member)
	}
	return out, rows.Err()
}

// MemberStatus reports heir membership and the current member count in one
// call so succession checks use a consistent community snapshot.
func (s *Service) MemberStatus(ctx context.Context, pubkey string) (bool, int, error) {
	var count int
	var member int
	if err := s.store.DB().QueryRowContext(ctx, `SELECT count(*), coalesce(max(pubkey=?),0) FROM community_members`, pubkey).Scan(&count, &member); err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return false, 0, nil
		}
		return false, 0, err
	}
	return member != 0, count, nil
}

// ModerationCounts returns the counters needed by the moderation view.
func (s *Service) ModerationCounts(ctx context.Context, start int64) (communityread.ModerationCounts, error) {
	var out communityread.ModerationCounts
	queries := []struct {
		table string
		to    *int
		where string
	}{
		{"community_bans", &out.Bans, "at>=?"},
		{"community_reports", &out.Reports, "at>=?"},
		{"community_reports", &out.Resolved, "resolved_at>=?"},
		{"community_event_bans", &out.Hidden, "at>=?"},
		{"community_ip_blocks", &out.BlockedAddresses, "at>=?"},
	}
	var failures error
	for _, query := range queries {
		if err := s.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM "+query.table+" WHERE "+query.where, start).Scan(query.to); err != nil {
			if strings.Contains(err.Error(), "no such table") {
				continue
			}
			failures = errors.Join(failures, err)
		}
	}
	return out, failures
}

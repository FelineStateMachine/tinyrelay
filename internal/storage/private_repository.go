package storage

import (
	"context"
	"fmt"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// HasPrivateRepositories checks the stored announcements, including legacy
// events. Multiword tags such as private are not in the NIP-01 tag index.
func (s *Store) HasPrivateRepositories(ctx context.Context) (bool, error) {
	return s.hasPrivateRepository(ctx, "", "")
}

// IsPrivateRepository resolves a state event's privacy from its announcement.
func (s *Store) IsPrivateRepository(ctx context.Context, pubkey, identifier string) (bool, error) {
	return s.hasPrivateRepository(ctx, pubkey, identifier)
}

func (s *Store) hasPrivateRepository(ctx context.Context, pubkey, identifier string) (bool, error) {
	query := `SELECT EXISTS(SELECT 1 FROM events e WHERE e.kind=?`
	args := []any{event.KIND_REPO}
	if pubkey != "" {
		query += ` AND e.pubkey=? AND e.d=?`
		args = append(args, pubkey, identifier)
	}
	query += ` AND EXISTS(SELECT 1 FROM json_each(e.raw, '$.tags') tag
		WHERE json_extract(tag.value, '$[0]')='private'
		AND lower(json_extract(tag.value, '$[1]'))='true'))`
	var found bool
	if err := s.DB().QueryRowContext(ctx, query, args...).Scan(&found); err != nil {
		return false, fmt.Errorf("check stored repository privacy: %w", err)
	}
	return found, nil
}

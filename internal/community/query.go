package community

import (
	"context"
	"database/sql"
	"errors"
)

func (s *Service) IsMember(ctx context.Context, pubkey string) (bool, error) {
	var exists bool
	err := s.store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM community_members WHERE pubkey=?)`, pubkey).Scan(&exists)
	return exists, err
}

func (s *Service) IsBanned(ctx context.Context, pubkey string) (bool, error) {
	var exists bool
	err := s.store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM community_bans WHERE pubkey=?)`, pubkey).Scan(&exists)
	return exists, err
}

func (s *Service) IsEventBanned(ctx context.Context, id string) (bool, error) {
	var exists bool
	err := s.store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM community_event_bans WHERE id=?)`, id).Scan(&exists)
	return exists, err
}

func (s *Service) IsIPBlocked(ctx context.Context, ip string) (bool, error) {
	var exists bool
	err := s.store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM community_ip_blocks WHERE ip=?)`, ip).Scan(&exists)
	return exists, err
}

func (s *Service) MemberByName(ctx context.Context, name string) (Member, error) {
	var member Member
	err := s.store.DB().QueryRowContext(ctx, `SELECT pubkey,name,note,role,invited_by,via,created_at,keep_days FROM community_members WHERE name=?`, name).Scan(&member.PubKey, &member.Name, &member.Note, &member.Role, &member.InvitedBy, &member.Via, &member.CreatedAt, &member.KeepDays)
	if errors.Is(err, sql.ErrNoRows) {
		return Member{}, nil
	}
	return member, err
}

func (s *Service) NIP05(ctx context.Context, name string, relayURL string) (map[string]any, error) {
	member, err := s.MemberByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if member.PubKey == "" {
		return map[string]any{"names": map[string]string{}, "relays": map[string][]string{}}, nil
	}
	return map[string]any{"names": map[string]string{name: member.PubKey}, "relays": map[string][]string{member.PubKey: {relayURL}}}, nil
}

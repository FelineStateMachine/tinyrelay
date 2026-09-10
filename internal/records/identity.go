package records

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func (s *Service) group() string { return s.groupID }

func groupMetaTags(group string, p policy.Policy) [][]string {
	tags := [][]string{{"-"}, {"d", group}, {"name", p.Name}, {"about", p.Description}}
	if p.Icon != "" {
		tags = append(tags, []string{"picture", p.Icon})
	}
	if p.Reads == "members" {
		tags = append(tags, []string{"private"})
	}
	if p.Writes != "open" {
		tags = append(tags, []string{"restricted"})
	}
	return tags
}

func loadOrCreateSecret(ctx context.Context, s *storage.Store) (string, error) {
	var secret string
	if err := s.GetSetting(ctx, secretSetting, &secret); err == nil {
		if len(secret) != 64 {
			return "", errors.New("records: invalid persisted relay secret")
		}
		return secret, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	secret, err := event.GenerateKey()
	if err != nil {
		return "", fmt.Errorf("records: generate relay key: %w", err)
	}
	if err := s.PutSetting(ctx, secretSetting, secret); err != nil {
		return "", err
	}
	return secret, nil
}

// PublicKey returns the relay signing key, never its secret.
func (s *Service) PublicKey() string {
	key, _ := event.PublicKey(s.secret)
	return key
}

func (s *Service) RelayURL() string { return s.relayURL }

func (s *Service) nextTimestamp(ctx context.Context, tx *sql.Tx, now int64) (int64, error) {
	var previous int64
	if err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='records.clock'`).Scan(&previous); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if now <= previous {
		now = previous + 1
	}
	if err := storage.PutSetting(ctx, tx, "records.clock", now); err != nil {
		return 0, err
	}
	return now, nil
}

func (s *Service) signed(ctx context.Context, kind int, tags [][]string, content string, now int64) (event.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out event.Event
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		t, err := s.nextTimestamp(ctx, tx, now)
		if err != nil {
			return err
		}
		out = event.Event{CreatedAt: t, Kind: kind, Tags: tags, Content: content}
		return event.Sign(&out, s.secret)
	})
	if err != nil {
		return event.Event{}, err
	}
	if s.onGenerated != nil {
		if err := s.onGenerated(ctx, out); err != nil {
			return event.Event{}, err
		}
	}
	return out, nil
}

func (s *Service) signedOnly(ctx context.Context, kind int, tags [][]string, content string, now int64) (event.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out event.Event
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		t, err := s.nextTimestamp(ctx, tx, now)
		if err != nil {
			return err
		}
		out = event.Event{CreatedAt: t, Kind: kind, Tags: tags, Content: content}
		return event.Sign(&out, s.secret)
	})
	return out, err
}

func (s *Service) Generate(ctx context.Context, kind int, tags [][]string, content string, now int64) (event.Event, error) {
	return s.signed(ctx, kind, tags, content, now)
}

func (s *Service) Sign(ctx context.Context, kind int, tags [][]string, content string, now int64) (event.Event, error) {
	return s.signedOnly(ctx, kind, tags, content, now)
}

func (s *Service) profile(ctx context.Context, now int64) (event.Event, error) {
	p := s.policy()
	b, _ := json.Marshal(map[string]string{"name": p.Name, "about": p.Description, "picture": p.Icon})
	return s.signed(ctx, event.KIND_PROFILE, nil, string(b), now)
}

func (s *Service) discovery(ctx context.Context, now int64) (event.Event, error) {
	p := s.policy()
	tags := [][]string{{"d", s.relayURL}}
	if p.Reads != "open" {
		tags = append(tags, []string{"r", p.Reads})
	}
	if p.Writes != "open" {
		tags = append(tags, []string{"writes", p.Writes})
	}
	return s.signed(ctx, event.KIND_RELAY_DISCOVERY, tags, s.relayURL, now)
}

func (s *Service) roster(ctx context.Context, now int64) (event.Event, error) {
	tags := [][]string{{"-"}}
	communityMembers, err := s.members(ctx)
	if err != nil {
		return event.Event{}, err
	}
	for _, member := range communityMembers {
		tag := []string{"member", member.PubKey}
		if member.Role != "" && member.Role != "member" {
			tag = append(tag, member.Role)
		}
		tags = append(tags, tag)
	}
	if len(tags) == 1 && s.policy().Owner != "" {
		tags = append(tags, []string{"member", s.policy().Owner, "owner"})
	}
	return s.signed(ctx, event.KIND_ROSTER, tags, "", now)
}

func (s *Service) groupState(ctx context.Context, now int64) (event.Event, error) {
	p := s.policy()
	tags := [][]string{{"-"}, {"d", s.group()}, {"name", p.Name}, {"about", p.Description}}
	if p.Icon != "" {
		tags = append(tags, []string{"picture", p.Icon})
	}
	if p.Reads == "members" {
		tags = append(tags, []string{"private"})
	}
	if p.Writes != "open" {
		tags = append(tags, []string{"restricted"})
	}
	return s.signed(ctx, event.KIND_GROUP_METADATA, tags, "", now)
}

func (s *Service) MembershipDelta(ctx context.Context, pubkey string, added bool, now int64) (event.Event, error) {
	kind := event.KIND_MEMBER_REMOVED
	if added {
		kind = event.KIND_MEMBER_ADDED
	}
	return s.signed(ctx, kind, [][]string{{"-"}, {"p", pubkey}}, "", now)
}

func (s *Service) groupRecords(ctx context.Context, now int64) ([]event.Event, error) {
	p := s.policy()
	communityMembers, err := s.members(ctx)
	if err != nil {
		return nil, err
	}
	var memberTags, admins, roleTags [][]string
	for _, member := range communityMembers {
		if member.Role == "agent" {
			memberTags = append(memberTags, []string{"p", member.PubKey, member.Role})
		} else {
			memberTags = append(memberTags, []string{"p", member.PubKey})
		}
		if member.Role == "owner" || member.Role == "moderator" {
			admins = append(admins, []string{"p", member.PubKey, member.Role})
		}
	}
	if !p.DirectoryPublic {
		memberTags = nil
	}
	for _, role := range roleOrder {
		roleTags = append(roleTags, []string{"role", role, rolesAbout[role]})
	}
	vals := []struct {
		kind    int
		tags    [][]string
		content string
	}{
		{event.KIND_GROUP_METADATA, groupMetaTags(s.group(), p), ""},
		{event.KIND_GROUP_ADMINS, append([][]string{{"-"}, {"d", s.group()}}, admins...), ""},
		{event.KIND_GROUP_MEMBERS, append([][]string{{"-"}, {"d", s.group()}}, memberTags...), ""},
		{event.KIND_GROUP_ROLES, append([][]string{{"-"}, {"d", s.group()}}, roleTags...), ""},
	}
	out := make([]event.Event, 0, len(vals)+3)
	for _, v := range vals {
		e, err := s.signed(ctx, v.kind, v.tags, v.content, now)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	for i, role := range roleOrder {
		e, err := s.signed(ctx, event.KIND_ROLE_DEF, [][]string{{"-"}, {"d", role}, {"label", role}, {"description", rolesAbout[role]}, {"order", strconv.Itoa(i + 1)}}, "", now)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *Service) groupRecordsEmpty(ctx context.Context, now int64, p policy.Policy) ([]event.Event, error) {
	_ = p
	vals := []struct {
		kind    int
		tags    [][]string
		content string
	}{
		{event.KIND_GROUP_METADATA, groupMetaTags(s.group(), p), ""},
		{event.KIND_GROUP_ADMINS, [][]string{{"-"}, {"d", s.group()}}, ""},
		{event.KIND_GROUP_MEMBERS, [][]string{{"-"}, {"d", s.group()}}, ""},
		{event.KIND_GROUP_ROLES, [][]string{{"-"}, {"d", s.group()}}, ""},
		{event.KIND_ROLE_DEF, [][]string{{"-"}, {"d", "owner"}, {"label", "owner"}, {"description", "relay owner"}, {"order", "1"}}, ""},
		{event.KIND_ROLE_DEF, [][]string{{"-"}, {"d", "moderator"}, {"label", "moderator"}, {"description", "relay moderator"}, {"order", "2"}}, ""},
		{event.KIND_ROLE_DEF, [][]string{{"-"}, {"d", "member"}, {"label", "member"}, {"description", "relay member"}, {"order", "3"}}, ""},
		{event.KIND_ROLE_DEF, [][]string{{"-"}, {"d", "agent"}, {"label", "agent"}, {"description", "agent under an owner-signed grant"}, {"order", "4"}}, ""},
	}
	out := make([]event.Event, 0, len(vals))
	for _, v := range vals {
		e, err := s.signed(ctx, v.kind, v.tags, v.content, now)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

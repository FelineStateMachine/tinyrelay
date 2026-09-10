package records

import "context"

// members is the sole records-side entry point for community membership data.
func (s *Service) members(ctx context.Context) ([]Member, error) {
	return s.community.Members(ctx)
}

func (s *Service) memberStatus(ctx context.Context, pubkey string) (bool, int, error) {
	return s.community.MemberStatus(ctx, pubkey)
}

func (s *Service) moderationCounts(ctx context.Context, start int64) (ModerationCounts, error) {
	return s.community.ModerationCounts(ctx, start)
}

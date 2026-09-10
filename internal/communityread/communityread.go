package communityread

type Member struct {
	// PubKey is the member's 32-byte public key in lowercase hexadecimal form.
	PubKey string
	// Name is the member's optional NIP-05-style local name.
	Name string
	// Role is the current community role.
	Role string
}

// ModerationCounts contains moderation activity counted from a caller-supplied
// Unix timestamp.
type ModerationCounts struct {
	Bans, Reports, Resolved, Hidden, BlockedAddresses int
}

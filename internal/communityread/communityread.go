// Package communityread contains the small read models shared by community
// and records. It has no storage or service dependencies.
package communityread

type Member struct {
	PubKey string
	Name   string
	Role   string
}

type ModerationCounts struct {
	Bans, Reports, Resolved, Hidden, BlockedAddresses int
}

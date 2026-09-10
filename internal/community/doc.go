// Package community owns the tenant's membership, moderation, invitations,
// rooms, agents and audit records.
//
// A Service stores community state in the tenant database and exposes direct
// operations plus transaction hooks for event ingestion. Methods whose names
// end in Tx and accept *sql.Tx use a transaction owned by the caller and leave
// commit or rollback to that caller. Event handlers whose callback accepts
// *sql.Tx open and close the transaction internally; the callback stores the
// accepted event in that transaction, so event, community state and
// projection work commit or roll back together.
//
// Community policy is read through the function supplied to ConfigurePolicy.
// Records consumes the narrow read model implemented by Members,
// MemberStatus and ModerationCounts, which keeps records independent of the
// community tables.
package community

// Package event keeps relay feature kind, job and room-reference semantics
// separate from the reusable values in protocol/nostr. Event and Filter are
// transitional aliases for the public types; parsing, encoding, signing and
// generic matching delegate to that package. Matches additionally preserves
// the host's private-kind search exclusion for existing consumers.
package event

// Package nostr provides signed Nostr events, JSON filters, canonical wire
// encoding, signature validation, tag helpers and standard kind ranges.
//
// Validate and Parse check the event wire shape, ID and signature. They do not
// apply kind-specific schemas or decide whether a host should accept an event.
// Matches evaluates filter constraints and content search without enforcing
// visibility. Callers must apply authorization and private-content policy.
//
// Canonical encodes a complete wire object; Sign and Validate use the NIP-01
// canonical array internally to derive and verify the event ID. Filter search
// uses case-insensitive content terms and ignores name:value tokens. It is not
// a ranked search engine.
package nostr

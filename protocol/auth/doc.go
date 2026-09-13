// Package auth verifies Nostr request proofs without deciding host access.
// It supports NIP-98 HTTP request proofs, NIP-42 relay challenges, BUD-11
// Blossom authorization and GRASP-08 repository proofs. Successful proof
// verification identifies a signer; callers must separately authorize that
// signer and apply resource, tenant, storage and hosting policy.
//
// Construct validators and challenge managers with their constructors. A nil
// clock uses time.Now. Their replay and challenge stores are in memory and
// scoped to each instance; reuse an instance across requests that share a
// replay boundary. The stores are protected for concurrent use. They do not
// persist across restarts or coordinate across processes.
//
// NIP-98 proofs are single-use within the one-minute replay window. Their URL
// binding preserves path escaping, port and query while ignoring scheme and
// hostname case. Methods compare case-insensitively. A nonempty body requires
// the matching payload hash. NIP-42 challenges are single-use and expire after
// one minute; relay URLs match exactly. Blossom proofs are reusable until
// their expiration and cannot be issued in the future. GRASP-08 GET proofs
// are reusable for supported Smart HTTP requests beneath the signed repository
// root, within the existing one-minute timestamp skew. Unlike NIP-98, GRASP-08
// requires an uppercase GET method and returns a uniform authentication error.
//
// Payload validation after streaming does not consume the proof again. It
// must follow request authentication and does not replace host authorization.
package auth

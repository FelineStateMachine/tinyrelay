// Package auth verifies the signed proofs used at HTTP and WebSocket
// boundaries. It handles NIP-98 request signatures, Blossom authorization,
// GRASP-08 repository proofs and NIP-42 challenges. Callers use the returned
// public key to apply relay policy and keep authorization state outside this
// package.
package auth

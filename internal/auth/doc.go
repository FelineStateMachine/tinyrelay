// Package auth is a transitional compatibility facade for protocol/auth.
// Its aliases share the public verifier types and replay state; proof parsing
// and validation live only in the public package. New consumers should import
// protocol/auth directly.
package auth

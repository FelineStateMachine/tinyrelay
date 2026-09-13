package event

import "github.com/FelineStateMachine/tinyrelay/protocol/nostr"

// Event is the public Nostr wire value. The alias keeps feature consumers
// source-compatible while they migrate to protocol/nostr.
type Event = nostr.Event

func Validate(e Event) error                     { return nostr.Validate(e) }
func Parse(raw []byte) (Event, error)            { return nostr.Parse(raw) }
func Canonical(e Event) ([]byte, error)          { return nostr.Canonical(e) }
func Tag(e Event, name string) string            { return nostr.Tag(e, name) }
func TagValues(e Event, name string) []string    { return nostr.TagValues(e, name) }
func Expiration(e Event) int64                   { return nostr.Expiration(e) }
func Difficulty(id string) int                   { return nostr.Difficulty(id) }
func CommittedDifficulty(e Event) int            { return nostr.CommittedDifficulty(e) }
func Sign(e *Event, secretHex string) error      { return nostr.Sign(e, secretHex) }
func GenerateKey() (string, error)               { return nostr.GenerateKey() }
func PublicKey(secretHex string) (string, error) { return nostr.PublicKey(secretHex) }
func IsEphemeral(k int) bool                     { return nostr.IsEphemeral(k) }
func IsReplaceable(k int) bool                   { return nostr.IsReplaceable(k) }
func IsAddressable(k int) bool                   { return nostr.IsAddressable(k) }

// IsPrivate is the host's recipient-scoped kind classification, not a generic
// Nostr range. Keep feature privacy decisions outside protocol/nostr.
func IsPrivate(k int) bool {
	return k == 4 || k == 13 || k == KIND_WRAP || k == 21059 || k == KIND_NOSTR_CONNECT || k == KIND_PUSH_REGISTRATION
}

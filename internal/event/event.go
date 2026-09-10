package event

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip13"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

type Event struct {
	ID        string     `json:"id"`
	PubKey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      int        `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
	Sig       string     `json:"sig"`
}

// Validate checks the wire shape, canonical ID and Schnorr signature of e.
// Kind-specific checks are applied by the protocol gate that owns the kind.
func Validate(e Event) error {
	if !hex64(e.ID) {
		return errors.New("invalid: bad id")
	}
	if !hex64(e.PubKey) {
		return errors.New("invalid: bad pubkey")
	}
	if len(e.Sig) != 128 || !isHex(e.Sig) {
		return errors.New("invalid: bad sig")
	}
	if e.Kind < 0 || e.Kind > 65535 {
		return errors.New("invalid: kind out of range")
	}
	if e.CreatedAt < 0 {
		return errors.New("invalid: bad created_at")
	}
	if e.Tags == nil {
		return errors.New("invalid: tags must be a list")
	}
	for _, tag := range e.Tags {
		if len(tag) == 0 {
			return errors.New("invalid: bad tag")
		}
	}
	serialized, err := serializeForID(e)
	if err != nil {
		return errors.New("invalid: cannot serialize event")
	}
	sum := sha256.Sum256(serialized)
	if e.ID != hex.EncodeToString(sum[:]) || !verifySignature(e, sum[:]) {
		return errors.New("invalid: id or signature does not match content")
	}
	return nil
}

// Parse decodes and validates one complete JSON event object.
func Parse(raw []byte) (Event, error) {
	if err := requireFields(raw); err != nil {
		return Event{}, err
	}
	var e Event
	if err := json.Unmarshal(raw, &e); err != nil {
		return Event{}, fmt.Errorf("invalid event JSON: %w", err)
	}
	if err := Validate(e); err != nil {
		return Event{}, err
	}
	return e, nil
}

// Canonical encodes e as the JSON representation used on relay boundaries.
func Canonical(e Event) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(e); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), nil
}

// Tag returns the first value for name, or an empty string when the tag is
// absent or has no value.
func Tag(e Event, name string) string {
	for _, tag := range e.Tags {
		if len(tag) > 0 && tag[0] == name && len(tag) > 1 {
			return tag[1]
		}
	}
	return ""
}

// TagValues returns every first value belonging to tags named name.
func TagValues(e Event, name string) []string {
	values := make([]string, 0)
	for _, tag := range e.Tags {
		if len(tag) > 1 && tag[0] == name {
			values = append(values, tag[1])
		}
	}
	return values
}

// Expiration returns the positive Unix timestamp in the expiration tag.
// Malformed, missing and nonpositive values return zero.
func Expiration(e Event) int64 {
	v, err := strconv.ParseInt(Tag(e, "expiration"), 10, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// Difficulty returns the NIP-13 proof-of-work difficulty encoded by id.
func Difficulty(id string) int {
	parsed, err := nostr.IDFromHex(id)
	if err != nil {
		return 0
	}
	return nip13.Difficulty(parsed)
}

// CommittedDifficulty returns the target in a nonce tag's third field.
func CommittedDifficulty(e Event) int {
	for _, tag := range e.Tags {
		if len(tag) > 2 && tag[0] == "nonce" {
			n, _ := strconv.Atoi(tag[2])
			return n
		}
	}
	return 0
}

// Sign derives the public key, ID and Schnorr signature for e from secretHex.
// A nil tag slice is encoded as an empty tag list before signing.
func Sign(e *Event, secretHex string) error {
	secret, err := decodeSecret(secretHex)
	if err != nil {
		return err
	}
	if e.Tags == nil {
		e.Tags = [][]string{}
	}
	key, pubkey := btcec.PrivKeyFromBytes(secret[:])
	e.PubKey = hex.EncodeToString(schnorr.SerializePubKey(pubkey))
	serialized, err := serializeForID(*e)
	if err != nil {
		return fmt.Errorf("serialize event: %w", err)
	}
	sum := sha256.Sum256(serialized)
	signature, err := schnorr.Sign(key, sum[:])
	if err != nil {
		return fmt.Errorf("sign event: %w", err)
	}
	e.ID = hex.EncodeToString(sum[:])
	e.Sig = hex.EncodeToString(signature.Serialize())
	return nil
}

// GenerateKey creates a random nonzero 32-byte private key in hexadecimal.
func GenerateKey() (string, error) {
	for {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return "", fmt.Errorf("generate private key: %w", err)
		}
		if !allZero(key) {
			return hex.EncodeToString(key), nil
		}
	}
}

// PublicKey derives the x-only Schnorr public key for secretHex.
func PublicKey(secretHex string) (string, error) {
	secret, err := decodeSecret(secretHex)
	if err != nil {
		return "", err
	}
	_, pubkey := btcec.PrivKeyFromBytes(secret[:])
	return hex.EncodeToString(schnorr.SerializePubKey(pubkey)), nil
}

// IsEphemeral reports whether k is in Nostr's ephemeral kind range.
func IsEphemeral(k int) bool { return k >= 20000 && k < 30000 }

// IsReplaceable reports whether k is a replaceable event kind.
func IsReplaceable(k int) bool { return k == 0 || k == 3 || k >= 10000 && k < 20000 }

// IsAddressable reports whether k is an addressable event kind.
func IsAddressable(k int) bool { return k >= 30000 && k < 40000 }

// IsPrivate reports whether k carries recipient-scoped content.
func IsPrivate(k int) bool {
	return k == 4 || k == 13 || k == KIND_WRAP || k == 21059 || k == KIND_NOSTR_CONNECT || k == KIND_PUSH_REGISTRATION
}

func hex64(s string) bool { return len(s) == 64 && isHex(s) }
func isHex(s string) bool {
	for _, char := range s {
		if char >= '0' && char <= '9' || char >= 'a' && char <= 'f' {
			continue
		}
		return false
	}
	return true
}

func serializeForID(e Event) ([]byte, error) {
	value := []any{0, e.PubKey, e.CreatedAt, e.Kind, e.Tags, e.Content}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	serialized := bytes.TrimSuffix(out.Bytes(), []byte{'\n'})
	serialized = bytes.ReplaceAll(serialized, []byte(`\u2028`), []byte("\u2028"))
	serialized = bytes.ReplaceAll(serialized, []byte(`\u2029`), []byte("\u2029"))
	return serialized, nil
}

func verifySignature(e Event, hash []byte) bool {
	pubkey, err := hex.DecodeString(e.PubKey)
	if err != nil {
		return false
	}
	key, err := schnorr.ParsePubKey(pubkey)
	if err != nil {
		return false
	}
	signature, err := hex.DecodeString(e.Sig)
	if err != nil {
		return false
	}
	sig, err := schnorr.ParseSignature(signature)
	return err == nil && sig.Verify(hash, key)
}

func decodeSecret(value string) ([32]byte, error) {
	var secret [32]byte
	if len(value) != 64 {
		return secret, errors.New("invalid secret key: expected 64 hex characters")
	}
	if _, err := hex.Decode(secret[:], []byte(strings.ToLower(value))); err != nil {
		return secret, fmt.Errorf("invalid secret key: %w", err)
	}
	if allZero(secret[:]) {
		return secret, errors.New("invalid secret key")
	}
	return secret, nil
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

func requireFields(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("invalid event JSON: %w", err)
	}
	for _, field := range []string{"id", "pubkey", "created_at", "kind", "tags", "content", "sig"} {
		if _, ok := fields[field]; !ok {
			return fmt.Errorf("invalid: missing %s", field)
		}
	}
	return nil
}

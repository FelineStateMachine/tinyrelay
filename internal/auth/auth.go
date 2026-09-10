package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

const tokenSkew = time.Minute

var ErrMissingAuthorization = errors.New("auth-required: missing Nostr Authorization header")

type Validator struct {
	now  func() time.Time
	mu   sync.Mutex
	seen map[string]time.Time
}

// NewValidator uses now to check proof freshness and track replay windows.
// A nil clock selects the system clock.
func NewValidator(now func() time.Time) *Validator {
	if now == nil {
		now = time.Now
	}
	return &Validator{now: now, seen: make(map[string]time.Time)}
}

// VerifyNIP98 validates a kind 27235 request proof against the exact URL,
// method and body supplied by the caller. Successfully verified event IDs are
// retained for the one-minute replay window.
func (v *Validator) VerifyNIP98(header, rawURL, method, body string) (event.Event, error) {
	e, err := decodeToken(header, "NIP-98")
	if err != nil {
		return event.Event{}, err
	}
	if err := event.Validate(e); err != nil {
		return event.Event{}, authError(err.Error())
	}
	if e.Kind != 27235 {
		return event.Event{}, authError("token must be kind 27235")
	}
	now := v.now()
	if outsideSkew(now, e.CreatedAt) {
		return event.Event{}, authError("token expired")
	}
	if err := sameRequestURL(event.Tag(e, "u"), rawURL); err != nil {
		return event.Event{}, err
	}
	if strings.ToUpper(event.Tag(e, "method")) != strings.ToUpper(method) {
		return event.Event{}, authError("token was signed for another method")
	}
	if body != "" && event.Tag(e, "payload") != hashBytes([]byte(body)) {
		return event.Event{}, authError("token payload hash does not match the body")
	}
	if err := v.mark(e.ID, now); err != nil {
		return event.Event{}, err
	}
	return e, nil
}

// VerifyBlossom validates a BUD-11 authorization for action. It accepts the
// protocol's reusable authorization form and checks its action and expiry.
func (v *Validator) VerifyBlossom(header, action string) (event.Event, error) {
	return v.verifyBlossom(header, action, "", "", false)
}

// VerifyBlossomRequest validates a BUD-11 authorization with optional server
// and blob-hash scope. Upload, delete, mirror and media actions require an x
// tag when this scoped form is used.
func (v *Validator) VerifyBlossomRequest(header, action, server, blobHash string) (event.Event, error) {
	return v.verifyBlossom(header, action, server, blobHash, true)
}

// IsBlossomAuthorization reports whether an Authorization header contains a
// signed BUD-11 event. Malformed tokens return false so callers can produce
// their normal authentication error through the protocol-specific verifier.
func IsBlossomAuthorization(header string) bool {
	e, err := decodeToken(header, "Blossom")
	return err == nil && e.Kind == 24242
}

func (v *Validator) verifyBlossom(header, action, server, blobHash string, strict bool) (event.Event, error) {
	e, err := decodeToken(header, "Blossom")
	if err != nil {
		return event.Event{}, err
	}
	if err := event.Validate(e); err != nil {
		return event.Event{}, authError(err.Error())
	}
	if e.Kind != 24242 {
		return event.Event{}, authError("token must be kind 24242")
	}
	if event.Tag(e, "t") != action {
		return event.Event{}, authError(fmt.Sprintf("token is not for %s", action))
	}
	if strict && server != "" && !blossomServerScope(e, server) {
		return event.Event{}, authError("token is not scoped to this server")
	}
	now := v.now()
	expiration := event.Expiration(e)
	if expiration == 0 || expiration <= now.Unix() {
		return event.Event{}, authError("token expired")
	}
	// BUD-11 authorization timestamps may not be from the future. The
	// expiration tag, rather than an arbitrary maximum age, controls how long
	// an otherwise valid token remains usable.
	if e.CreatedAt > now.Unix() {
		return event.Event{}, authError("token is from the future")
	}
	if strict && requiresBlobScope(action) && len(event.TagValues(e, "x")) == 0 {
		return event.Event{}, authError("token must include an x tag")
	}
	if strict && blobHash != "" && len(event.TagValues(e, "x")) > 0 && !contains(event.TagValues(e, "x"), blobHash) {
		return event.Event{}, authError("token x tag does not name this blob")
	}
	return e, nil
}

func requiresBlobScope(action string) bool {
	switch action {
	case "upload", "delete", "mirror", "media":
		return true
	default:
		return false
	}
}

func blossomServerScope(e event.Event, server string) bool {
	for _, value := range event.TagValues(e, "server") {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(server)) {
			return true
		}
	}
	return len(event.TagValues(e, "server")) == 0
}

func (v *Validator) WhoAsks(header, rawURL, method, body, action, blobHash string) ([]string, error) {
	if strings.TrimSpace(header) == "" {
		return nil, nil
	}
	e, err := decodeToken(header, "authorization")
	if err != nil {
		return nil, err
	}
	if e.Kind == 24242 {
		if action == "" {
			return nil, authError("this door takes a NIP-98 signature")
		}
		e, err = v.VerifyBlossom(header, action)
		if err != nil {
			return nil, err
		}
		xs := event.TagValues(e, "x")
		if blobHash != "" && len(xs) > 0 && !contains(xs, blobHash) {
			return nil, authError("token x tag does not name this blob")
		}
		return []string{e.PubKey}, nil
	}
	e, err = v.VerifyNIP98(header, rawURL, method, body)
	if err != nil {
		return nil, err
	}
	return []string{e.PubKey}, nil
}

// ValidateBlobPayload checks the hash binding on a token that was already
// authenticated by WhoAsks. It deliberately does not mark the event as seen a
// second time: upload authorization and the streamed body validator inspect
// the same proof at two different points in the request.
func (v *Validator) ValidateBlobPayload(header, actualSHA string) error {
	e, err := decodeToken(header, "authorization")
	if err != nil {
		return err
	}
	if err := event.Validate(e); err != nil {
		return authError(err.Error())
	}
	switch e.Kind {
	case 24242:
		if values := event.TagValues(e, "x"); len(values) > 0 && !contains(values, actualSHA) {
			return authError("token x tag does not name this blob")
		}
	case 27235:
		if event.Tag(e, "payload") != actualSHA {
			return authError("token payload hash does not match the blob hash")
		}
	default:
		return authError("token must be kind 24242 or 27235")
	}
	return nil
}

// ValidateBlossomPayload rechecks the content hash for a Blossom-authenticated
// operation after a streaming body has been materialized. It does not mark the
// authorization event as seen a second time.
func (v *Validator) ValidateBlossomPayload(header, action, actualSHA string) error {
	e, err := v.VerifyBlossom(header, action)
	if err != nil {
		return err
	}
	if values := event.TagValues(e, "x"); len(values) > 0 && !contains(values, actualSHA) {
		return authError("token x tag does not name this blob")
	}
	return nil
}

type ChallengeManager struct {
	relayURL string
	now      func() time.Time
	mu       sync.Mutex
	active   map[string]time.Time
}

// NewChallengeManager creates a NIP-42 challenge store for relayURL. Issued
// challenges are single-use and expire with the validator replay window.
func NewChallengeManager(relayURL string, now func() time.Time) *ChallengeManager {
	if now == nil {
		now = time.Now
	}
	return &ChallengeManager{relayURL: relayURL, now: now, active: make(map[string]time.Time)}
}

// Issue creates and stores a cryptographically random NIP-42 challenge.
func (m *ChallengeManager) Issue() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("create NIP-42 challenge: %w", err)
	}
	challenge := hex.EncodeToString(raw)
	now := m.now()
	m.mu.Lock()
	m.prune(now)
	m.active[challenge] = now.Add(tokenSkew)
	m.mu.Unlock()
	return challenge, nil
}

// Verify validates and consumes a NIP-42 AUTH event for the configured relay.
func (m *ChallengeManager) Verify(e event.Event) error {
	if err := event.Validate(e); err != nil {
		return authError(err.Error())
	}
	if e.Kind != 22242 {
		return authError("AUTH event must be kind 22242")
	}
	now := m.now()
	if outsideSkew(now, e.CreatedAt) {
		return authError("AUTH event timestamp expired")
	}
	if event.Tag(e, "relay") != m.relayURL {
		return authError("AUTH event names another relay")
	}
	challenge := event.Tag(e, "challenge")
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune(now)
	if _, ok := m.active[challenge]; !ok {
		return authError("AUTH challenge is unknown or already used")
	}
	delete(m.active, challenge)
	return nil
}

func (m *ChallengeManager) prune(now time.Time) {
	for challenge, expires := range m.active {
		if !expires.After(now) {
			delete(m.active, challenge)
		}
	}
}

func (v *Validator) mark(id string, now time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	for seenID, at := range v.seen {
		if now.Sub(at) > tokenSkew {
			delete(v.seen, seenID)
		}
	}
	if _, ok := v.seen[id]; ok {
		return authError("token replay detected")
	}
	v.seen[id] = now
	return nil
}

func decodeToken(header, tokenName string) (event.Event, error) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Nostr") {
		return event.Event{}, ErrMissingAuthorization
	}
	raw, err := decodeBase64(parts[1])
	if err != nil {
		return event.Event{}, authError(fmt.Sprintf("malformed %s token", tokenName))
	}
	e, err := event.Parse(raw)
	if err != nil {
		return event.Event{}, authError(err.Error())
	}
	return e, nil
}

func decodeBase64(value string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err := encoding.DecodeString(value); err == nil {
			return raw, nil
		}
	}
	return nil, errors.New("invalid base64")
}

func sameRequestURL(signed, requested string) error {
	want, ok := normalizedRequestURL(requested)
	if !ok {
		return authError("token was signed for another URL")
	}
	got, ok := normalizedRequestURL(signed)
	if !ok || got != want {
		return authError("token was signed for another URL")
	}
	return nil
}

func normalizedRequestURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	// URL.Host preserves a possibly meaningful port while hostname matching is
	// case insensitive. Query strings remain part of the NIP-98 binding.
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" {
		host += ":" + port
	}
	return strings.ToLower(u.Scheme) + "://" + host + path + "?" + u.RawQuery, true
}

func outsideSkew(now time.Time, createdAt int64) bool {
	delta := now.Unix() - createdAt
	return delta > int64(tokenSkew/time.Second) || delta < -int64(tokenSkew/time.Second)
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func authError(reason string) error { return fmt.Errorf("auth-required: %s", reason) }
func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

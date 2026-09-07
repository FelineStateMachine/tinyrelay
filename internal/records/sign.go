package records

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"net/url"
	"strings"
	"time"
)

// GenerateEphemeral signs a relay-owned response without invoking the
// durable records callback. It is used for protocol responses such as the
// NIP-43 invite request, which must be relay-signed but never stored.
func (s *Service) GenerateEphemeral(ctx context.Context, kind int, tags [][]string, content string, now int64) (event.Event, error) {
	if err := ctx.Err(); err != nil {
		return event.Event{}, err
	}
	if now == 0 {
		now = time.Now().Unix()
	}
	// Protocol proofs use wall-clock time. The immutable relay key is safe to
	// read concurrently; taking the projection lock here would invert the
	// router/records lock order for responses generated inside a REQ.
	out := event.Event{CreatedAt: now, Kind: kind, Tags: tags, Content: content}
	if err := event.Sign(&out, s.secret); err != nil {
		return event.Event{}, err
	}
	return out, nil
}

// SignNIP98 creates a short-lived HTTP authorization proof with the durable
// relay identity. The caller supplies the exact URL Git will request and the
// body hash when the method has a payload.
func (s *Service) SignNIP98(ctx context.Context, method, rawURL, payloadHash string, now int64) (event.Event, error) {
	if _, err := url.ParseRequestURI(rawURL); err != nil || !strings.Contains(rawURL, "://") {
		return event.Event{}, fmt.Errorf("records: invalid NIP-98 URL: %w", err)
	}
	secret, err := event.GenerateKey()
	if err != nil {
		return event.Event{}, err
	}
	tags := [][]string{{"u", rawURL}, {"method", strings.ToUpper(method)}, {"nonce", secret}}
	if payloadHash != "" {
		if len(payloadHash) != sha256.Size*2 {
			return event.Event{}, fmt.Errorf("records: invalid NIP-98 payload hash")
		}
		if _, err := hex.DecodeString(payloadHash); err != nil {
			return event.Event{}, fmt.Errorf("records: invalid NIP-98 payload hash: %w", err)
		}
		tags = append(tags, []string{"payload", payloadHash})
	}
	if now == 0 {
		now = time.Now().Unix()
	}
	return s.GenerateEphemeral(ctx, 27235, tags, "", now)
}

// SignNIP42 answers a relay challenge without persisting an AUTH event.
func (s *Service) SignNIP42(ctx context.Context, relayURL, challenge string, now int64) (event.Event, error) {
	if relayURL == "" || challenge == "" {
		return event.Event{}, fmt.Errorf("records: missing NIP-42 challenge")
	}
	return s.GenerateEphemeral(ctx, event.KIND_AUTH, [][]string{{"relay", relayURL}, {"challenge", challenge}}, "", now)
}

package records

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func (s *Service) notify(ctx context.Context, typ, owner, target string, now int64) error {
	if !s.policy().Notify.Succession && (typ == "succession" || typ == "succession-transfer") {
		return nil
	}
	content, _ := json.Marshal(map[string]string{"type": typ, "target": target})
	wrapped, err := s.giftWrap(owner, string(content), "relay", now)
	if err != nil {
		return err
	}
	if s.deliverNotification != nil {
		if err := s.deliverNotification(ctx, wrapped, target); err != nil {
			return err
		}
		return s.push(ctx, target, typ, "relay", "")
	}
	if s.onGenerated != nil {
		return s.onGenerated(ctx, wrapped)
	}
	return err
}

func (s *Service) push(ctx context.Context, recipient, kind, subject, text string) error {
	if s.pushNotification == nil {
		return nil
	}
	return s.pushNotification(ctx, recipient, kind, subject, text)
}

// Notify applies notification preferences and emits a durable gift wrap.
func (s *Service) Notify(ctx context.Context, kind, text, subject, target string, now int64) error {
	p := s.policy()
	if kind != "test" {
		enabled := map[string]bool{"reports": p.Notify.Reports, "jobs": p.Notify.Jobs, "succession": p.Notify.Succession, "digest": p.Notify.Digest}[kind]
		if !enabled {
			return nil
		}
	}
	if target == "" {
		target = p.Owner
	}
	return s.notifyText(ctx, kind, text, subject, target, now)
}

func (s *Service) notifyText(ctx context.Context, kind, text, subject, target string, now int64) error {
	if target == "" {
		return errors.New("notify: empty recipient")
	}
	if s.deliverNotification == nil && s.onGenerated == nil {
		return nil
	}
	wrapped, err := s.giftWrap(target, text, subject, now)
	if err != nil {
		return err
	}
	if s.deliverNotification != nil {
		if err := s.deliverNotification(ctx, wrapped, target); err != nil {
			return err
		}
		return s.push(ctx, target, kind, subject, text)
	}
	if s.onGenerated != nil {
		return s.onGenerated(ctx, wrapped)
	}
	return nil
}

// giftWrap creates the NIP-17 and NIP-59 records.
func (s *Service) giftWrap(recipient, content, subject string, now int64) (event.Event, error) {
	rpk, err := nostr.PubKeyFromHex(recipient)
	if err != nil {
		return event.Event{}, err
	}
	sk, err := nostr.SecretKeyFromHex(s.secret)
	if err != nil {
		return event.Event{}, err
	}
	// Build the three NIP-59 records with the project's canonical event signer.
	// This avoids the upstream nostr JSON encoder's unsafe fast path under -race.
	tags := [][]string{{"p", recipient}}
	if subject != "" {
		tags = append(tags, []string{"subject", subject})
	}
	rumor := event.Event{CreatedAt: now, Kind: event.KIND_DM, Tags: tags, Content: content}
	if err := event.Sign(&rumor, s.secret); err != nil {
		return event.Event{}, err
	}
	rumor.Sig = strings.Repeat("0", 128)
	rumorRaw, err := json.Marshal(rumor)
	if err != nil {
		return event.Event{}, err
	}
	key, err := nip44.GenerateConversationKey(rpk, sk)
	if err != nil {
		return event.Event{}, err
	}
	rumorCipher, err := nip44.Encrypt(string(rumorRaw), key)
	if err != nil {
		return event.Event{}, err
	}
	seal := event.Event{CreatedAt: now, Kind: 13, Tags: [][]string{}, Content: rumorCipher}
	if err := event.Sign(&seal, s.secret); err != nil {
		return event.Event{}, err
	}
	nonce := nostr.Generate()
	outerKey, err := nip44.GenerateConversationKey(rpk, nonce)
	if err != nil {
		return event.Event{}, err
	}
	sealRaw, err := json.Marshal(seal)
	if err != nil {
		return event.Event{}, err
	}
	outerCipher, err := nip44.Encrypt(string(sealRaw), outerKey)
	if err != nil {
		return event.Event{}, err
	}
	wrapped := event.Event{CreatedAt: now, Kind: event.KIND_WRAP, Tags: [][]string{{"p", recipient}}, Content: outerCipher}
	if err := event.Sign(&wrapped, nonce.Hex()); err != nil {
		return event.Event{}, err
	}
	return wrapped, nil
}

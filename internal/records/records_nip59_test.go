//go:build !race

package records

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"
	"fiatjaf.com/nostr/nip59"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestNotificationIsNIP17NIP59Unwrapable(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "tenant.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ownerSecret := "0101010101010101010101010101010101010101010101010101010101010101"
	owner, err := event.PublicKey(ownerSecret)
	if err != nil {
		t.Fatal(err)
	}
	p := policy.Defaults(owner)
	var got event.Event
	r, err := New(ctx, Config{Community: emptyCommunityReader{}, Store: s, Policy: func() policy.Policy { return p }, RelayURL: "wss://relay", OnGenerated: func(_ context.Context, e event.Event) error { got = e; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Notify(ctx, "test", "hello", "subject", owner, 100); err != nil {
		t.Fatal(err)
	}
	if got.Kind != event.KIND_WRAP {
		t.Fatalf("want gift wrap, got %d", got.Kind)
	}
	sk, err := nostr.SecretKeyFromHex(ownerSecret)
	if err != nil {
		t.Fatal(err)
	}
	sigBytes, _ := hex.DecodeString(got.Sig)
	var sig [64]byte
	copy(sig[:], sigBytes)
	gw := nostr.Event{ID: nostr.MustIDFromHex(got.ID), PubKey: nostr.MustPubKeyFromHex(got.PubKey), CreatedAt: nostr.Timestamp(got.CreatedAt), Kind: nostr.Kind(got.Kind), Content: got.Content, Sig: sig}
	for _, tag := range got.Tags {
		gw.Tags = append(gw.Tags, nostr.Tag(tag))
	}
	rumor, err := nip59.GiftUnwrap(gw, func(other nostr.PubKey, ciphertext string) (string, error) {
		key, err := nip44.GenerateConversationKey(other, sk)
		if err != nil {
			return "", err
		}
		return nip44.Decrypt(ciphertext, key)
	})
	if err != nil {
		t.Fatal(err)
	}
	if rumor.Kind != nostr.KindDirectMessage || rumor.Content != "hello" {
		t.Fatalf("unexpected rumor: kind=%d content=%q", rumor.Kind, rumor.Content)
	}
}

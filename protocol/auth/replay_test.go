package auth

import (
	"encoding/base64"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
)

func TestNIP98ReplayWindowBoundary(t *testing.T) {
	now := time.Unix(1700000000, 0)
	v := NewValidator(func() time.Time { return now })
	// A future-dated proof is still fresh after its first replay window ends.
	e := signedEvent(t, 27235, now.Unix()+60, "", [][]string{{"u", "https://relay.example/api"}, {"method", "GET"}})
	header := token(t, e)
	if _, err := v.VerifyNIP98(header, "https://relay.example/api", "GET", ""); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := v.VerifyNIP98(header, "https://relay.example/api", "GET", ""); err == nil || !strings.Contains(err.Error(), "replay") {
		t.Fatalf("proof must remain seen at exactly one minute: %v", err)
	}
	now = now.Add(time.Second)
	if _, err := v.VerifyNIP98(header, "https://relay.example/api", "GET", ""); err != nil {
		t.Fatalf("expired replay entry should be pruned: %v", err)
	}
}

func TestNIP98AcceptsAllExistingBase64Forms(t *testing.T) {
	now := time.Unix(1700000000, 0)
	e := signedEvent(t, 27235, now.Unix(), "\u083e\u083f", [][]string{{"u", "https://relay.example/api"}, {"method", "GET"}})
	raw, err := nostr.Canonical(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		v := NewValidator(func() time.Time { return now })
		header := "nOsTr " + encoding.EncodeToString(raw)
		if _, err := v.VerifyNIP98(header, "https://relay.example/api", "GET", ""); err != nil {
			t.Fatalf("base64 form rejected: %v", err)
		}
	}
}

func TestNIP42ChallengeExpiresAtOneMinute(t *testing.T) {
	now := time.Unix(1700000000, 0)
	m := NewChallengeManager("wss://relay.example", func() time.Time { return now })
	challenge, err := m.Issue()
	if err != nil {
		t.Fatal(err)
	}
	e := signedEvent(t, 22242, now.Unix(), "", [][]string{{"relay", "wss://relay.example"}, {"challenge", challenge}})
	now = now.Add(time.Minute)
	if err := m.Verify(e); err == nil || !strings.Contains(err.Error(), "challenge") {
		t.Fatalf("challenge must expire even while timestamp remains fresh: %v", err)
	}
}

func TestConcurrentProofConsumptionIsSingleUse(t *testing.T) {
	now := time.Unix(1700000000, 0)
	v := NewValidator(func() time.Time { return now })
	e := signedEvent(t, 27235, now.Unix(), "", [][]string{{"u", "https://relay.example/api"}, {"method", "GET"}})
	header := token(t, e)
	m := NewChallengeManager("wss://relay.example", func() time.Time { return now })
	challenge, err := m.Issue()
	if err != nil {
		t.Fatal(err)
	}
	authEvent := signedEvent(t, 22242, now.Unix(), "", [][]string{{"relay", "wss://relay.example"}, {"challenge", challenge}})
	for name, verify := range map[string]func() error{
		"NIP-98": func() error { _, err := v.VerifyNIP98(header, "https://relay.example/api", "GET", ""); return err },
		"NIP-42": func() error { return m.Verify(authEvent) },
	} {
		t.Run(name, func(t *testing.T) {
			var successes atomic.Int32
			var wg sync.WaitGroup
			for range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if verify() == nil {
						successes.Add(1)
					}
				}()
			}
			wg.Wait()
			if got := successes.Load(); got != 1 {
				t.Fatalf("successful consumers = %d, want 1", got)
			}
		})
	}
}

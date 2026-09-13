package auth_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	legacy "github.com/FelineStateMachine/tinyrelay/internal/auth"
	"github.com/FelineStateMachine/tinyrelay/protocol/auth"
	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
)

func TestCompatibilitySharesReplayStateAndErrors(t *testing.T) {
	if legacy.ErrMissingAuthorization != auth.ErrMissingAuthorization || legacy.ErrGRASP08Unauthorized != auth.ErrGRASP08Unauthorized {
		t.Fatal("compatibility facade changed error identity")
	}
	now := time.Unix(1700000000, 0)
	old := legacy.NewValidator(func() time.Time { return now })
	var public *auth.Validator = old
	e := nostr.Event{Kind: 27235, CreatedAt: now.Unix(), Tags: [][]string{{"u", "https://relay.example"}, {"method", "GET"}}}
	if err := nostr.Sign(&e, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	raw, err := nostr.Canonical(e)
	if err != nil {
		t.Fatal(err)
	}
	header := "Nostr " + base64.StdEncoding.EncodeToString(raw)
	if _, err := old.VerifyNIP98(header, "https://relay.example", "GET", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := public.VerifyNIP98(header, "https://relay.example", "GET", ""); err == nil || !strings.Contains(err.Error(), "replay") {
		t.Fatalf("facade lost replay state: %v", err)
	}
	var manager *auth.ChallengeManager = legacy.NewChallengeManager("wss://relay.example", nil)
	if _, err := manager.Issue(); err != nil {
		t.Fatal(err)
	}
}

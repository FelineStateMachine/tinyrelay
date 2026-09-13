package consumer_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/protocol/auth"
	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
)

func TestPublicProofVerifierReplayContracts(t *testing.T) {
	now := time.Unix(1700000000, 0)
	v := auth.NewValidator(func() time.Time { return now })
	sign := func(kind int, tags [][]string) (nostr.Event, string) {
		t.Helper()
		e := nostr.Event{Kind: kind, CreatedAt: now.Unix(), Tags: tags}
		if err := nostr.Sign(&e, strings.Repeat("0", 63)+"1"); err != nil {
			t.Fatal(err)
		}
		raw, err := nostr.Canonical(e)
		if err != nil {
			t.Fatal(err)
		}
		return e, "Nostr " + base64.StdEncoding.EncodeToString(raw)
	}
	_, header := sign(27235, [][]string{{"u", "https://relay.example/api?a=1"}, {"method", "GET"}})
	if _, err := v.VerifyNIP98(header, "https://relay.example/api?a=2", "GET", ""); err == nil {
		t.Fatal("query mismatch accepted")
	}
	var proof nostr.Event
	var err error
	proof, err = v.VerifyNIP98(header, "https://RELAY.example/api?a=1", "get", "")
	if err != nil || proof.PubKey == "" {
		t.Fatalf("NIP-98 proof: %v", err)
	}
	if _, err := v.VerifyNIP98(header, "https://relay.example/api?a=1", "GET", ""); err == nil || !strings.Contains(err.Error(), "replay") {
		t.Fatalf("NIP-98 replay: %v", err)
	}
	if _, err := v.VerifyNIP98("", "https://relay.example/api", "GET", ""); !errors.Is(err, auth.ErrMissingAuthorization) {
		t.Fatalf("missing header: %v", err)
	}

	_, header = sign(24242, [][]string{{"t", "get"}, {"expiration", "1700000300"}})
	if !auth.IsBlossomAuthorization(header) {
		t.Fatal("Blossom classification")
	}
	for range 2 {
		if _, err := v.VerifyBlossomRequest(header, "get", "relay.example", ""); err != nil {
			t.Fatalf("reusable Blossom: %v", err)
		}
	}
	root := "https://relay.example/owner/repo.git"
	_, header = sign(27235, [][]string{{"u", root}, {"method", "GET"}})
	for _, suffix := range []string{"/info/refs?service=git-upload-pack", "/git-upload-pack"} {
		if _, err := v.VerifyGRASP08(header, root+suffix); err != nil {
			t.Fatalf("reusable GRASP-08: %v", err)
		}
	}
	if _, err := v.VerifyGRASP08(header, root+"/objects"); err != auth.ErrGRASP08Unauthorized {
		t.Fatalf("GRASP-08 error identity: %v", err)
	}
	m := auth.NewChallengeManager("wss://relay.example", func() time.Time { return now })
	challenge, err := m.Issue()
	if err != nil {
		t.Fatal(err)
	}
	e, _ := sign(22242, [][]string{{"relay", "wss://relay.example"}, {"challenge", challenge}})
	if err := m.Verify(e); err != nil {
		t.Fatal(err)
	}
	if err := m.Verify(e); err == nil {
		t.Fatal("NIP-42 challenge reused")
	}
}

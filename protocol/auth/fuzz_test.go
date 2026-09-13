package auth

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
)

// FuzzNIP98Boundary sends arbitrary authorization inputs through the complete
// decoder and verifier. Invalid proofs are expected; panics are not.
func FuzzNIP98Boundary(f *testing.F) {
	f.Add("", "https://relay.example/", "GET", "")
	f.Add("Nostr !!!", "https://relay.example/api", "POST", "body")
	f.Add("nIp98 AA==", "not a URL", "", "\x00")
	secret := strings.Repeat("0", 63) + "1"
	proof := nostr.Event{CreatedAt: 1_700_000_000, Kind: 27235, Tags: [][]string{{"u", "https://relay.example/api"}, {"method", "POST"}}, Content: "seed"}
	if err := nostr.Sign(&proof, secret); err != nil {
		f.Fatal(err)
	}
	raw, err := nostr.Canonical(proof)
	if err != nil {
		f.Fatal(err)
	}
	header := "Nostr " + base64.RawStdEncoding.EncodeToString(raw)
	f.Add(header, "https://relay.example/api", "POST", "")
	f.Fuzz(func(t *testing.T, header, rawURL, method, body string) {
		validator := NewValidator(func() time.Time { return time.Unix(1_700_000_000, 0) })
		_, err := validator.VerifyNIP98(header, rawURL, method, body)
		if header == "Nostr "+base64.RawStdEncoding.EncodeToString(raw) && rawURL == "https://relay.example/api" && method == "POST" && body == "" && err != nil {
			t.Fatalf("valid seeded proof rejected: %v", err)
		}
	})
}

func FuzzNIP98Binding(f *testing.F) {
	secret := strings.Repeat("0", 63) + "1"
	proof := nostr.Event{CreatedAt: 1_700_000_000, Kind: 27235, Tags: [][]string{{"u", "https://relay.example/api"}, {"method", "POST"}, {"payload", hashBytes([]byte("body"))}}, Content: "binding seed"}
	if err := nostr.Sign(&proof, secret); err != nil {
		f.Fatal(err)
	}
	raw, err := nostr.Canonical(proof)
	if err != nil {
		f.Fatal(err)
	}
	header := "Nostr " + base64.RawStdEncoding.EncodeToString(raw)
	f.Add("https://relay.example/api", "POST", "body")
	f.Add("https://relay.example/other", "POST", "body")
	f.Add("https://relay.example/api", "GET", "body")
	f.Add("https://relay.example/api", "POST", "other")
	f.Fuzz(func(t *testing.T, rawURL, method, body string) {
		validator := NewValidator(func() time.Time { return time.Unix(1_700_000_000, 0) })
		_, err := validator.VerifyNIP98(header, rawURL, method, body)
		if rawURL == "https://relay.example/api" && method == "POST" && body == "body" && err != nil {
			t.Fatalf("valid binding rejected: %v", err)
		}
		if (rawURL != "https://relay.example/api" || method != "POST" || body != "body") && err == nil {
			t.Fatal("accepted mismatched NIP-98 binding")
		}
	})
}

// FuzzBlossomBoundary covers the alternate authorization scheme and its
// server/blob scope parameters.
func FuzzBlossomBoundary(f *testing.F) {
	f.Add("", "upload", "https://relay.example", "")
	f.Add("Blossom !!!", "delete", "relay", "hash")
	secret := strings.Repeat("0", 63) + "1"
	proof := nostr.Event{CreatedAt: 1_700_000_000, Kind: 24242, Tags: [][]string{{"t", "upload"}, {"expiration", "1700000300"}, {"server", "https://relay.example"}, {"x", strings.Repeat("a", 64)}}, Content: "seed"}
	if err := nostr.Sign(&proof, secret); err != nil {
		f.Fatal(err)
	}
	raw, err := nostr.Canonical(proof)
	if err != nil {
		f.Fatal(err)
	}
	header := "Nostr " + base64.RawStdEncoding.EncodeToString(raw)
	f.Add(header, "upload", "https://relay.example", strings.Repeat("a", 64))
	f.Fuzz(func(t *testing.T, header, action, server, blobHash string) {
		validator := NewValidator(func() time.Time { return time.Unix(1_700_000_000, 0) })
		_, err := validator.VerifyBlossomRequest(header, action, server, blobHash)
		if header == "Nostr "+base64.RawStdEncoding.EncodeToString(raw) && action == "upload" && server == "https://relay.example" && blobHash == strings.Repeat("a", 64) && err != nil {
			t.Fatalf("valid seeded authorization rejected: %v", err)
		}
	})
}

func FuzzBlossomBinding(f *testing.F) {
	secret := strings.Repeat("0", 63) + "1"
	blobHash := strings.Repeat("a", 64)
	proof := nostr.Event{CreatedAt: 1_700_000_000, Kind: 24242, Tags: [][]string{{"t", "upload"}, {"expiration", "1700000300"}, {"server", "https://relay.example"}, {"x", blobHash}}, Content: "binding seed"}
	if err := nostr.Sign(&proof, secret); err != nil {
		f.Fatal(err)
	}
	raw, err := nostr.Canonical(proof)
	if err != nil {
		f.Fatal(err)
	}
	header := "Nostr " + base64.RawStdEncoding.EncodeToString(raw)
	f.Add("upload", "https://relay.example", blobHash)
	f.Add("delete", "https://relay.example", blobHash)
	f.Add("upload", "https://other.example", blobHash)
	f.Add("upload", "https://relay.example", strings.Repeat("b", 64))
	f.Fuzz(func(t *testing.T, action, server, hash string) {
		validator := NewValidator(func() time.Time { return time.Unix(1_700_000_000, 0) })
		_, err := validator.VerifyBlossomRequest(header, action, server, hash)
		if action == "upload" && server == "https://relay.example" && hash == blobHash && err != nil {
			t.Fatalf("valid binding rejected: %v", err)
		}
		mismatch := action != "upload" || server != "" && server != "https://relay.example" || hash != "" && hash != blobHash
		if mismatch && err == nil {
			t.Fatal("accepted mismatched Blossom binding")
		}
	})
}

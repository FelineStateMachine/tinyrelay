package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestNIP98ValidatesRequestAndRejectsReplay(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	validator := NewValidator(func() time.Time { return now })
	e := signedEvent(t, 27235, now.Unix(), "", [][]string{{"u", "https://relay.example/api?ignored=1"}, {"method", "POST"}, {"payload", ""}})
	// A non-empty request body must carry its exact SHA-256 tag.
	e = signedEvent(t, 27235, now.Unix(), "", [][]string{{"u", "https://relay.example/api?ignored=1"}, {"method", "POST"}, {"payload", sha256Hex("hello")}})
	header := token(t, e)
	got, err := validator.VerifyNIP98(header, "https://RELAY.example/api?ignored=1", "post", "hello")
	if err != nil || got.PubKey == "" {
		t.Fatalf("verify valid token: %v", err)
	}
	if _, err := validator.VerifyNIP98(header, "https://relay.example/api?ignored=1", "POST", "hello"); err == nil || !strings.Contains(err.Error(), "replay") {
		t.Fatalf("expected replay rejection, got %v", err)
	}
}

func TestNIP98ExpiryAndPayload(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	validator := NewValidator(func() time.Time { return now })
	e := signedEvent(t, 27235, now.Add(-61*time.Second).Unix(), "", [][]string{{"u", "https://relay.example/api"}, {"method", "POST"}, {"payload", sha256Hex("hello")}})
	if _, err := validator.VerifyNIP98(token(t, e), "https://relay.example/api", "POST", "hello"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expiry, got %v", err)
	}
	e = signedEvent(t, 27235, now.Unix(), "", [][]string{{"u", "https://relay.example/api"}, {"method", "POST"}, {"payload", sha256Hex("other")}})
	if _, err := validator.VerifyNIP98(token(t, e), "https://relay.example/api", "POST", "hello"); err == nil || !strings.Contains(err.Error(), "payload") {
		t.Fatalf("expected payload rejection, got %v", err)
	}
}

func TestNIP98BindsSchemeQueryAndPath(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	validator := NewValidator(func() time.Time { return now })
	for _, requested := range []string{
		"http://relay.example/api?a=1",
		"https://relay.example/other?a=1",
		"https://relay.example/api?a=2",
		"https://relay.example/api?a=1#fragment",
		"https://user:pass@relay.example/api?a=1",
	} {
		e := signedEvent(t, 27235, now.Unix(), "", [][]string{{"u", "https://relay.example/api?a=1"}, {"method", "GET"}})
		if _, err := validator.VerifyNIP98(token(t, e), requested, "GET", ""); err == nil {
			t.Fatalf("accepted request URL %q", requested)
		}
	}
}

func TestBlossomTokenMayBeReusedAcrossHeadAndPut(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	validator := NewValidator(func() time.Time { return now })
	e := signedEvent(t, 24242, now.Unix(), "upload", [][]string{{"t", "upload"}, {"expiration", "1700000300"}})
	header := token(t, e)
	if _, err := validator.VerifyBlossom(header, "upload"); err != nil {
		t.Fatalf("verify first Blossom request: %v", err)
	}
	if _, err := validator.VerifyBlossom(header, "upload"); err != nil {
		t.Fatalf("verify reused Blossom request: %v", err)
	}
}

func TestValidateBlobPayloadBindsBlossomAndNIP98Hashes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	validator := NewValidator(func() time.Time { return now })
	hash := sha256Hex("blob")
	blossom := signedEvent(t, 24242, now.Unix(), "upload", [][]string{{"t", "upload"}, {"expiration", "1700000300"}, {"x", hash}})
	if err := validator.ValidateBlobPayload(token(t, blossom), hash); err != nil {
		t.Fatalf("valid Blossom hash: %v", err)
	}
	if err := validator.ValidateBlobPayload(token(t, blossom), sha256Hex("other")); err == nil || !strings.Contains(err.Error(), "blob") {
		t.Fatalf("expected Blossom hash rejection, got %v", err)
	}
	nip98 := signedEvent(t, 27235, now.Unix(), "", [][]string{{"u", "https://relay.example/upload"}, {"method", "PUT"}, {"payload", hash}})
	if err := validator.ValidateBlobPayload(token(t, nip98), hash); err != nil {
		t.Fatalf("valid NIP98 hash: %v", err)
	}
	if err := validator.ValidateBlobPayload(token(t, nip98), sha256Hex("other")); err == nil || !strings.Contains(err.Error(), "payload") {
		t.Fatalf("expected NIP98 hash rejection, got %v", err)
	}
}

func TestValidateBlossomPayloadBindsMirrorHash(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	validator := NewValidator(func() time.Time { return now })
	hash := sha256Hex("blob")
	tokenEvent := signedEvent(t, 24242, now.Unix(), "", [][]string{{"t", "mirror"}, {"expiration", "1700000300"}, {"x", hash}})
	header := token(t, tokenEvent)
	if err := validator.ValidateBlossomPayload(header, "mirror", hash); err != nil {
		t.Fatalf("valid mirror hash: %v", err)
	}
	if err := validator.ValidateBlossomPayload(header, "mirror", sha256Hex("other")); err == nil {
		t.Fatal("accepted mismatched mirror hash")
	}
}

func TestVerifyBlossomRequestRequiresScopeAndServer(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	validator := NewValidator(func() time.Time { return now })
	hash := sha256Hex("blob")
	valid := signedEvent(t, 24242, now.Unix(), "", [][]string{{"t", "upload"}, {"expiration", "1700000300"}, {"server", "relay.example"}, {"x", hash}})
	if _, err := validator.VerifyBlossomRequest(token(t, valid), "upload", "relay.example", hash); err != nil {
		t.Fatalf("valid scoped Blossom token: %v", err)
	}
	wrongServer := signedEvent(t, 24242, now.Unix(), "", [][]string{{"t", "upload"}, {"expiration", "1700000300"}, {"server", "other.example"}, {"x", hash}})
	if _, err := validator.VerifyBlossomRequest(token(t, wrongServer), "upload", "relay.example", hash); err == nil {
		t.Fatal("accepted token scoped to another server")
	}
	missingHash := signedEvent(t, 24242, now.Unix(), "", [][]string{{"t", "upload"}, {"expiration", "1700000300"}, {"server", "relay.example"}})
	if _, err := validator.VerifyBlossomRequest(token(t, missingHash), "upload", "relay.example", ""); err == nil {
		t.Fatal("accepted upload token without x scope")
	}
}

func TestVerifyBlossomRequestAllowsUnscopedGet(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	validator := NewValidator(func() time.Time { return now })
	hash := sha256Hex("blob")
	e := signedEvent(t, 24242, now.Add(-24*time.Hour).Unix(), "", [][]string{{"t", "get"}, {"expiration", "1700086400"}})
	if _, err := validator.VerifyBlossomRequest(token(t, e), "get", "relay.example", hash); err != nil {
		t.Fatalf("unscoped GET token rejected: %v", err)
	}
}

func TestVerifyBlossomRequestRejectsFutureCreatedAt(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	validator := NewValidator(func() time.Time { return now })
	e := signedEvent(t, 24242, now.Add(time.Second).Unix(), "", [][]string{{"t", "get"}, {"expiration", "1700000300"}})
	if _, err := validator.VerifyBlossomRequest(token(t, e), "get", "relay.example", ""); err == nil || !strings.Contains(err.Error(), "future") {
		t.Fatalf("future token accepted: %v", err)
	}
}

func TestChallengeManagerBindsRelayAndConsumesChallenge(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := NewChallengeManager("wss://relay.example/tenant", func() time.Time { return now })
	challenge, err := m.Issue()
	if err != nil {
		t.Fatalf("issue challenge: %v", err)
	}
	e := signedEvent(t, 22242, now.Unix(), "", [][]string{{"relay", "wss://relay.example/tenant"}, {"challenge", challenge}})
	if err := m.Verify(e); err != nil {
		t.Fatalf("verify challenge: %v", err)
	}
	if err := m.Verify(e); err == nil || !strings.Contains(err.Error(), "challenge") {
		t.Fatalf("expected one-time challenge rejection, got %v", err)
	}
}

func signedEvent(t *testing.T, kind int, createdAt int64, content string, tags [][]string) event.Event {
	t.Helper()
	e := event.Event{CreatedAt: createdAt, Kind: kind, Content: content, Tags: tags}
	if err := event.Sign(&e, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatalf("sign event: %v", err)
	}
	return e
}

func token(t *testing.T, e event.Event) string {
	t.Helper()
	raw, err := event.Canonical(e)
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	return "Nostr " + base64.StdEncoding.EncodeToString(raw)
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

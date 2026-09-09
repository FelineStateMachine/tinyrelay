package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func subscriber(t *testing.T) (Subscription, *ecdh.PrivateKey, []byte) {
	t.Helper()
	private, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	rand.Read(auth)
	var subscription Subscription
	subscription.Endpoint = "https://push.example/send/abc"
	subscription.Keys.P256dh = base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes())
	subscription.Keys.Auth = base64.RawURLEncoding.EncodeToString(auth)
	return subscription, private, auth
}

// decrypt mirrors the browser side of RFC 8291 so the test proves the body
// is readable by a real subscriber.
func decrypt(t *testing.T, body []byte, receiver *ecdh.PrivateKey, auth []byte) []byte {
	t.Helper()
	salt := body[:16]
	recordSize := binary.BigEndian.Uint32(body[16:20])
	idLength := int(body[20])
	senderPublic := body[21 : 21+idLength]
	record := body[21+idLength:]
	if recordSize != 4096 || idLength != 65 {
		t.Fatalf("header rs=%d idlen=%d", recordSize, idLength)
	}
	sender, err := ecdh.P256().NewPublicKey(senderPublic)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := receiver.ECDH(sender)
	if err != nil {
		t.Fatal(err)
	}
	keyInfo := append(append([]byte("WebPush: info\x00"), receiver.PublicKey().Bytes()...), senderPublic...)
	ikm, _ := hkdf.Key(sha256.New, shared, auth, string(keyInfo), 32)
	key, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, record, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain[len(plain)-1] != 2 {
		t.Fatalf("missing final record delimiter: %x", plain[len(plain)-1])
	}
	return plain[:len(plain)-1]
}

func TestEncryptRoundTripsForASubscriber(t *testing.T) {
	subscription, private, auth := subscriber(t)
	if err := subscription.Validate(); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"title":"tiny","body":"hello"}`)
	first, err := Encrypt(subscription, payload)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := Encrypt(subscription, payload)
	if bytes.Equal(first, second) {
		t.Fatal("encryption reused its salt or ephemeral key")
	}
	if got := decrypt(t, first, private, auth); !bytes.Equal(got, payload) {
		t.Fatalf("decrypted %q", got)
	}
	if _, err := Encrypt(subscription, bytes.Repeat([]byte("x"), 4000)); err == nil {
		t.Fatal("oversized payload accepted")
	}
}

func TestVAPIDAuthorizationVerifies(t *testing.T) {
	keys, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := keys.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ParseKeys(encoded)
	if err != nil || restored.PublicKey() != keys.PublicKey() {
		t.Fatalf("restore keys: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	header, err := keys.Authorization("https://push.example/send/abc?x=1", "mailto:owner@example", now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(header, "vapid t=") || !strings.Contains(header, ", k="+keys.PublicKey()) {
		t.Fatalf("header %q", header)
	}
	token := strings.TrimPrefix(strings.SplitN(header, ", k=", 2)[0], "vapid t=")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q", token)
	}
	claimsRaw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err := json.Unmarshal(claimsRaw, &claims); err != nil || claims["aud"] != "https://push.example" || claims["sub"] != "mailto:owner@example" || int64(claims["exp"].(float64)) != now.Add(12*time.Hour).Unix() {
		t.Fatalf("claims %s %v", claimsRaw, err)
	}
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	public, _ := base64.RawURLEncoding.DecodeString(keys.PublicKey())
	x, y := elliptic.Unmarshal(elliptic.P256(), public)
	if !ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		t.Fatal("JWT signature does not verify")
	}
}

func TestSendReportsGoneSubscriptions(t *testing.T) {
	keys, _ := GenerateKeys()
	subscription, _, _ := subscriber(t)
	var headers http.Header
	status := 201
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		w.WriteHeader(status)
	}))
	defer server.Close()
	subscription.Endpoint = server.URL + "/send/abc"
	result, err := Send(context.Background(), server.Client(), keys, "mailto:owner@example", subscription, []byte(`{"title":"t"}`), time.Hour)
	if err != nil || result.Status != 201 || result.Gone {
		t.Fatalf("send: %v %+v", err, result)
	}
	if headers.Get("Content-Encoding") != "aes128gcm" || headers.Get("TTL") != "3600" || !strings.HasPrefix(headers.Get("Authorization"), "vapid t=") {
		t.Fatalf("headers %v", headers)
	}
	status = http.StatusGone
	result, err = Send(context.Background(), server.Client(), keys, "mailto:owner@example", subscription, []byte(`{}`), time.Hour)
	if err == nil || !result.Gone {
		t.Fatalf("gone: %v %+v", err, result)
	}
}

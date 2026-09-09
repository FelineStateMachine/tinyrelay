// Package webpush sends Web Push messages: RFC 8291 payload encryption with
// the aes128gcm content encoding and RFC 8292 VAPID authorization. It uses
// only the standard library so the relay keeps its dependency footprint.
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
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Subscription is what the browser's PushManager hands to the page.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// Validate checks the shape of a subscription before it is stored.
func (s Subscription) Validate() error {
	parsed, err := url.Parse(s.Endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || len(s.Endpoint) > 2048 {
		return errors.New("invalid: push endpoint must be an HTTPS URL")
	}
	if key, err := decode(s.Keys.P256dh); err != nil || len(key) != 65 || key[0] != 4 {
		return errors.New("invalid: push p256dh key must be an uncompressed P-256 point")
	}
	if auth, err := decode(s.Keys.Auth); err != nil || len(auth) != 16 {
		return errors.New("invalid: push auth secret must be 16 bytes")
	}
	return nil
}

// Keys is the VAPID key pair identifying this relay to push services.
type Keys struct{ private *ecdsa.PrivateKey }

// GenerateKeys creates a fresh P-256 VAPID key pair.
func GenerateKeys() (Keys, error) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Keys{}, err
	}
	return Keys{private: private}, nil
}

// Marshal serializes the private key for the settings table.
func (k Keys) Marshal() (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k.private)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(der), nil
}

// ParseKeys restores a key pair from Marshal output.
func ParseKeys(encoded string) (Keys, error) {
	der, err := decode(encoded)
	if err != nil {
		return Keys{}, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return Keys{}, err
	}
	private, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || private.Curve != elliptic.P256() {
		return Keys{}, errors.New("webpush: VAPID key is not a P-256 key")
	}
	return Keys{private: private}, nil
}

// PublicKey is the applicationServerKey the browser subscribes with.
func (k Keys) PublicKey() string {
	return base64.RawURLEncoding.EncodeToString(elliptic.Marshal(elliptic.P256(), k.private.PublicKey.X, k.private.PublicKey.Y))
}

// Authorization builds the VAPID header for one push service origin. The
// token is an ES256 JWT with the endpoint origin as its audience.
func (k Keys) Authorization(endpoint, subject string, now time.Time) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(map[string]any{"aud": parsed.Scheme + "://" + parsed.Host, "exp": now.Add(12 * time.Hour).Unix(), "sub": subject})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, k.private, digest[:])
	if err != nil {
		return "", err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	token := signing + "." + base64.RawURLEncoding.EncodeToString(signature)
	return "vapid t=" + token + ", k=" + k.PublicKey(), nil
}

// Encrypt produces an aes128gcm body for the subscription. Each call uses a
// fresh ephemeral key and salt, so the same payload never encrypts twice
// the same way.
func Encrypt(subscription Subscription, plaintext []byte) ([]byte, error) {
	if len(plaintext) > 3993 {
		return nil, errors.New("webpush: payload exceeds one record")
	}
	receiverPublic, err := decode(subscription.Keys.P256dh)
	if err != nil {
		return nil, err
	}
	auth, err := decode(subscription.Keys.Auth)
	if err != nil {
		return nil, err
	}
	curve := ecdh.P256()
	receiver, err := curve.NewPublicKey(receiverPublic)
	if err != nil {
		return nil, fmt.Errorf("webpush: subscriber key: %w", err)
	}
	ephemeral, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := ephemeral.ECDH(receiver)
	if err != nil {
		return nil, err
	}
	senderPublic := ephemeral.PublicKey().Bytes()
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	return encrypt(plaintext, shared, receiverPublic, senderPublic, auth, salt)
}

func encrypt(plaintext, shared, receiverPublic, senderPublic, auth, salt []byte) ([]byte, error) {
	keyInfo := append(append([]byte("WebPush: info\x00"), receiverPublic...), senderPublic...)
	ikm, err := hkdf.Key(sha256.New, shared, auth, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	contentKey, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(contentKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	record := gcm.Seal(nil, nonce, append(append([]byte{}, plaintext...), 2), nil)
	var body bytes.Buffer
	body.Write(salt)
	_ = binary.Write(&body, binary.BigEndian, uint32(4096))
	body.WriteByte(byte(len(senderPublic)))
	body.Write(senderPublic)
	body.Write(record)
	return body.Bytes(), nil
}

// Result reports the push service's answer for one message.
type Result struct {
	Status int
	// Gone means the subscription no longer exists and should be removed.
	Gone bool
}

// Send delivers one encrypted message. A nil client uses http.DefaultClient;
// callers that reach the public internet should pass a client that refuses
// private addresses.
func Send(ctx context.Context, client *http.Client, keys Keys, subject string, subscription Subscription, payload []byte, ttl time.Duration) (Result, error) {
	body, err := Encrypt(subscription, payload)
	if err != nil {
		return Result{}, err
	}
	authorization, err := keys.Authorization(subscription.Endpoint, subject, time.Now())
	if err != nil {
		return Result{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, subscription.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("Content-Encoding", "aes128gcm")
	request.Header.Set("TTL", strconv.Itoa(int(ttl.Seconds())))
	request.Header.Set("Urgency", "normal")
	request.Header.Set("Authorization", authorization)
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return Result{}, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	result := Result{Status: response.StatusCode, Gone: response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return result, fmt.Errorf("webpush: push service returned HTTP %d", response.StatusCode)
	}
	return result, nil
}

func decode(value string) ([]byte, error) {
	if raw, err := base64.RawURLEncoding.DecodeString(value); err == nil {
		return raw, nil
	}
	if raw, err := base64.URLEncoding.DecodeString(value); err == nil {
		return raw, nil
	}
	return base64.StdEncoding.DecodeString(value)
}

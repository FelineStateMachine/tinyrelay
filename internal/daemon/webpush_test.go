package daemon

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/webpush"
	"github.com/FelineStateMachine/tinyrelay/internal/work"
)

func testSubscription(t *testing.T, endpoint string) webpush.Subscription {
	t.Helper()
	private, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	rand.Read(auth)
	var subscription webpush.Subscription
	subscription.Endpoint = endpoint
	subscription.Keys.P256dh = base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes())
	subscription.Keys.Auth = base64.RawURLEncoding.EncodeToString(auth)
	return subscription
}

func TestPushSubscriptionRequiresSignatureAndDeliversNotifications(t *testing.T) {
	a, tenant := testTenant(t)
	received := make(chan http.Header, 4)
	status := http.StatusCreated
	service := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		w.WriteHeader(status)
	}))
	defer service.Close()
	tenant.pushClient = service.Client()
	subscription := testSubscription(t, service.URL+"/send/device-1")
	body, _ := json.Marshal(subscription)

	keyReq := httptest.NewRequest("GET", "http://relay.test/push/key", nil)
	keyRes := httptest.NewRecorder()
	a.ServeHTTP(keyRes, keyReq)
	var key map[string]string
	if keyRes.Code != 200 || json.NewDecoder(keyRes.Body).Decode(&key) != nil || len(key["key"]) < 80 {
		t.Fatalf("push key %d %s", keyRes.Code, keyRes.Body.String())
	}

	unsigned := httptest.NewRequest("POST", "http://relay.test/push/subscribe", strings.NewReader(string(body)))
	unsignedRes := httptest.NewRecorder()
	a.ServeHTTP(unsignedRes, unsigned)
	if unsignedRes.Code != 401 {
		t.Fatalf("unsigned subscribe %d", unsignedRes.Code)
	}

	signed := httptest.NewRequest("POST", "http://relay.test/push/subscribe", strings.NewReader(string(body)))
	signRequest(t, signed, string(body))
	signedRes := httptest.NewRecorder()
	a.ServeHTTP(signedRes, signed)
	if signedRes.Code != 200 {
		t.Fatalf("subscribe %d %s", signedRes.Code, signedRes.Body.String())
	}

	ctx := context.Background()
	owner := tenant.Policy().Owner
	if err := tenant.enqueuePush(ctx, owner, "test", "relay", "hello from the relay"); err != nil {
		t.Fatal(err)
	}
	var payload string
	if err := tenant.store.DB().QueryRowContext(ctx, "SELECT payload FROM work_intents WHERE kind=? AND target=?", notificationPush, owner).Scan(&payload); err != nil {
		t.Fatalf("push intent not queued: %v", err)
	}
	if err := tenant.handleNotificationPush(ctx, work.Intent{Kind: notificationPush, Target: owner, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	headers := <-received
	if headers.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(headers.Get("Authorization"), "vapid t=") || headers.Get("TTL") == "" {
		t.Fatalf("push headers %v", headers)
	}

	// A subscription the push service reports gone is removed.
	status = http.StatusGone
	if err := tenant.handleNotificationPush(ctx, work.Intent{Kind: notificationPush, Target: owner, Payload: payload}); err != nil {
		t.Fatalf("gone device should not fail the intent: %v", err)
	}
	<-received
	var remaining int
	if err := tenant.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM web_push").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("gone subscription kept: %d %v", remaining, err)
	}
	// Without devices, summaries are not queued at all.
	before := countIntents(t, tenant, notificationPush)
	if err := tenant.enqueuePush(ctx, owner, "test", "relay", ""); err != nil {
		t.Fatal(err)
	}
	if countIntents(t, tenant, notificationPush) != before {
		t.Fatal("push intent queued without a device")
	}
}

func countIntents(t *testing.T, tenant *Tenant, kind string) int {
	t.Helper()
	var count int
	if err := tenant.store.DB().QueryRowContext(context.Background(), "SELECT count(*) FROM work_intents WHERE kind=?", kind).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

package daemon

// Web Push delivers relay notifications to installed browsers. A member
// registers a device subscription with a signed request; the relay keeps one
// VAPID key pair per tenant and sends each notification summary encrypted to
// every registered device. Push services see only ciphertext.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/webpush"
	"github.com/FelineStateMachine/tinyrelay/internal/work"
	"github.com/google/uuid"
)

const (
	pushSettingKey       = "webpush.vapid"
	pushMaxPerPubkey     = 8
	pushMaxFailures      = 5
	pushTTL              = 24 * time.Hour
	pushSummaryMaxLength = 160
)

const pushSchema = `CREATE TABLE IF NOT EXISTS web_push(endpoint TEXT PRIMARY KEY, pubkey TEXT NOT NULL, subscription TEXT NOT NULL, created_at INTEGER NOT NULL, failures INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS web_push_pubkey ON web_push(pubkey);`

type pushPayload struct {
	Recipient string `json:"recipient"`
	Kind      string `json:"kind"`
	Subject   string `json:"subject"`
	Text      string `json:"text"`
}

// pushMessage is what the service worker receives after decryption.
type pushMessage struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
}

func (t *Tenant) initPush(ctx context.Context) error {
	_, err := t.store.DB().ExecContext(ctx, pushSchema)
	return err
}

// pushVAPIDKeys returns the tenant's VAPID pair, creating it on first use.
func (t *Tenant) pushVAPIDKeys(ctx context.Context) (webpush.Keys, error) {
	t.pushMu.Lock()
	defer t.pushMu.Unlock()
	if t.pushVAPID != nil {
		return *t.pushVAPID, nil
	}
	var encoded string
	err := t.store.GetSetting(ctx, pushSettingKey, &encoded)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return webpush.Keys{}, err
	}
	var keys webpush.Keys
	if encoded != "" {
		keys, err = webpush.ParseKeys(encoded)
	} else {
		keys, err = webpush.GenerateKeys()
		if err == nil {
			encoded, err = keys.Marshal()
		}
		if err == nil {
			err = t.store.PutSetting(ctx, pushSettingKey, encoded)
		}
	}
	if err != nil {
		return webpush.Keys{}, err
	}
	t.pushVAPID = &keys
	return keys, nil
}

// pushSubject identifies the relay operator to push services.
func (t *Tenant) pushSubject() string {
	if contact := strings.TrimSpace(t.Policy().Contact); contact != "" && strings.Contains(contact, "@") && !strings.Contains(contact, ":") {
		return "mailto:" + contact
	}
	return t.publicURL
}

func (t *Tenant) pushHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/push/key" && r.Method == http.MethodGet:
		keys, err := t.pushVAPIDKeys(r.Context())
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "push keys unavailable"})
			return
		}
		writeJSON(w, 200, map[string]string{"key": keys.PublicKey()})
	case (r.URL.Path == "/push/subscribe" || r.URL.Path == "/push/unsubscribe") && r.Method == http.MethodPost:
		// Registration is a signed mutation like every other change made
		// from the browser; the session cookie alone is not enough.
		actor, err := t.resolveUIActor(r)
		if err != nil || actor == "" {
			writeJSON(w, 401, map[string]string{"error": "auth-required: sign this request"})
			return
		}
		body, err := t.requestBody(w, r)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		var subscription webpush.Subscription
		if err := json.Unmarshal(body, &subscription); err != nil || subscription.Validate() != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid: push subscription"})
			return
		}
		if r.URL.Path == "/push/unsubscribe" {
			if _, err := t.store.DB().ExecContext(r.Context(), "DELETE FROM web_push WHERE endpoint=? AND pubkey=?", subscription.Endpoint, actor); err != nil {
				writeJSON(w, 500, map[string]string{"error": "unsubscribe failed"})
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
		if role, err := t.community.Role(r.Context(), actor); err != nil || (role == "" && actor != t.Policy().Owner) {
			writeJSON(w, 403, map[string]string{"error": "restricted: notifications are limited to relay members"})
			return
		}
		if err := t.savePushSubscription(r.Context(), actor, subscription); err != nil {
			writeJSON(w, statusForPush(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

func statusForPush(err error) int {
	if strings.HasPrefix(err.Error(), "invalid:") {
		return 400
	}
	return 500
}

func (t *Tenant) savePushSubscription(ctx context.Context, actor string, subscription webpush.Subscription) error {
	raw, err := json.Marshal(subscription)
	if err != nil {
		return err
	}
	return t.store.WithTx(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM web_push WHERE pubkey=? AND endpoint<>?", actor, subscription.Endpoint).Scan(&count); err != nil {
			return err
		}
		if count >= pushMaxPerPubkey {
			return fmt.Errorf("invalid: at most %d devices per key", pushMaxPerPubkey)
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO web_push(endpoint,pubkey,subscription,created_at,failures) VALUES(?,?,?,?,0) ON CONFLICT(endpoint) DO UPDATE SET pubkey=excluded.pubkey, subscription=excluded.subscription, failures=0", subscription.Endpoint, actor, string(raw), time.Now().Unix())
		return err
	})
}

// enqueuePush records one notification summary for device delivery. It is
// suitable for records.Config.PushNotification and does nothing for
// recipients without registered devices.
func (t *Tenant) enqueuePush(ctx context.Context, recipient, kind, subject, text string) error {
	var count int
	if err := t.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM web_push WHERE pubkey=?", recipient).Scan(&count); err != nil || count == 0 {
		return err
	}
	if len(text) > pushSummaryMaxLength {
		text = text[:pushSummaryMaxLength]
	}
	payload, err := json.Marshal(pushPayload{Recipient: recipient, Kind: kind, Subject: subject, Text: text})
	if err != nil {
		return err
	}
	return t.store.WithTx(ctx, func(tx *sql.Tx) error {
		return storage.AddIntents(ctx, tx, []storage.Intent{{Kind: notificationPush, EventID: uuid.NewString(), Target: recipient, Payload: string(payload)}}, time.Now().Unix())
	})
}

func (t *Tenant) handleNotificationPush(ctx context.Context, intent work.Intent) error {
	var payload pushPayload
	if err := json.Unmarshal([]byte(intent.Payload), &payload); err != nil || payload.Recipient != intent.Target {
		return errors.New("notification-push: invalid payload")
	}
	rows, err := t.store.DB().QueryContext(ctx, "SELECT endpoint, subscription, failures FROM web_push WHERE pubkey=?", payload.Recipient)
	if err != nil {
		return err
	}
	type device struct {
		endpoint     string
		subscription webpush.Subscription
		failures     int
	}
	var devices []device
	for rows.Next() {
		var item device
		var raw string
		if err := rows.Scan(&item.endpoint, &raw, &item.failures); err != nil {
			rows.Close()
			return err
		}
		if json.Unmarshal([]byte(raw), &item.subscription) == nil {
			devices = append(devices, item)
		}
	}
	rows.Close()
	if len(devices) == 0 {
		return nil
	}
	keys, err := t.pushVAPIDKeys(ctx)
	if err != nil {
		return err
	}
	message, err := json.Marshal(t.pushMessage(payload))
	if err != nil {
		return err
	}
	client := t.pushClient
	if client == nil {
		client = replication.NewPinnedClient(10 * time.Second)
	}
	var result error
	for _, item := range devices {
		sent, sendErr := webpush.Send(ctx, client, keys, t.pushSubject(), item.subscription, message, pushTTL)
		switch {
		case sendErr == nil:
			_, _ = t.store.DB().ExecContext(ctx, "UPDATE web_push SET failures=0 WHERE endpoint=?", item.endpoint)
		case sent.Gone || item.failures+1 >= pushMaxFailures:
			_, _ = t.store.DB().ExecContext(ctx, "DELETE FROM web_push WHERE endpoint=?", item.endpoint)
		default:
			_, _ = t.store.DB().ExecContext(ctx, "UPDATE web_push SET failures=failures+1 WHERE endpoint=?", item.endpoint)
			result = errors.Join(result, sendErr)
		}
	}
	return result
}

// pushMessage turns a notification summary into what the device shows.
func (t *Tenant) pushMessage(payload pushPayload) pushMessage {
	title := t.Policy().Name
	if title == "" {
		title = t.meta.Name
	}
	body := payload.Text
	if body == "" {
		body = map[string]string{
			"reports": "A new report needs moderation.", "jobs": "A background job finished.",
			"succession": "Relay succession changed.", "digest": "Your relay digest is ready.",
			"test": "This is a test notification.", "succession-transfer": "Relay ownership was transferred.",
		}[payload.Kind]
	}
	if body == "" {
		body = "New relay notification."
	}
	if payload.Subject != "" && payload.Subject != "relay" && !strings.EqualFold(payload.Subject, payload.Kind) {
		title = title + ": " + payload.Subject
	}
	return pushMessage{Title: title, Body: body, URL: strings.TrimRight(t.publicURL, "/") + "/inbox", Tag: "tiny-" + payload.Kind}
}

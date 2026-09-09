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

	"github.com/FelineStateMachine/tinyrelay/internal/relay"
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

const pushSchema = `CREATE TABLE IF NOT EXISTS web_push(endpoint TEXT PRIMARY KEY, pubkey TEXT NOT NULL, subscription TEXT NOT NULL, created_at INTEGER NOT NULL, failures INTEGER NOT NULL DEFAULT 0, categories TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS web_push_pubkey ON web_push(pubkey);`

type pushPayload struct {
	Recipient string `json:"recipient"`
	// Kind is a device category for event summaries or a relay notice kind.
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
	URL     string `json:"url,omitempty"`
}

// pushMessage is what the service worker receives after decryption.
type pushMessage struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
	Badge int    `json:"badge"`
}

// pushRegistration is the browser's subscription with its chosen categories.
type pushRegistration struct {
	Subscription webpush.Subscription `json:"subscription"`
	Categories   []string             `json:"categories"`
}

func (t *Tenant) initPush(ctx context.Context) error {
	if _, err := t.store.DB().ExecContext(ctx, pushSchema); err != nil {
		return err
	}
	// Devices registered before categories existed receive everything.
	_, err := t.store.DB().ExecContext(ctx, "ALTER TABLE web_push ADD COLUMN categories TEXT NOT NULL DEFAULT ''")
	if err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return err
	}
	return nil
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

// pushHTTP serves the device notification endpoints under one named
// operation so the diagnostics registry and traces see them as a unit.
func (t *Tenant) pushHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, finish := t.app.telemetry.Start(r.Context(), "push")
	r = r.WithContext(ctx)
	outcome := "error"
	defer func() { finish(outcome) }()
	outcome = t.servePush(w, r)
}

// servePush returns the telemetry outcome for the request it answered.
func (t *Tenant) servePush(w http.ResponseWriter, r *http.Request) string {
	switch {
	case r.URL.Path == "/push/key" && r.Method == http.MethodGet:
		keys, err := t.pushVAPIDKeys(r.Context())
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "push keys unavailable"})
			return "error"
		}
		writeJSON(w, 200, map[string]string{"key": keys.PublicKey()})
		return "ok"
	case r.URL.Path == "/inbox/seen" && r.Method == http.MethodPost:
		// Opening the inbox clears the badge. This is the person's own
		// read marker, so the browser session may set it from this origin.
		actor, err := t.resolveUIActor(r)
		if err != nil || actor == "" {
			if s, sessionErr := t.cookieReadSession(r, relay.Session{RelayURL: t.RelayURL()}); sessionErr == nil {
				actor = first(s.PubKeys)
			}
		}
		if actor == "" {
			writeJSON(w, 401, map[string]string{"error": "auth-required: sign this request"})
			return "unauthorized"
		}
		if err := t.markInboxSeen(r.Context(), actor); err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not record inbox visit"})
			return "error"
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return "ok"
	case (r.URL.Path == "/push/subscribe" || r.URL.Path == "/push/unsubscribe") && r.Method == http.MethodPost:
		// Registration is a signed mutation like every other change made
		// from the browser; the session cookie alone is not enough.
		actor, err := t.resolveUIActor(r)
		if err != nil || actor == "" {
			writeJSON(w, 401, map[string]string{"error": "auth-required: sign this request"})
			return "unauthorized"
		}
		body, err := t.requestBody(w, r)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return "invalid"
		}
		registration, err := parsePushRegistration(body)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid: push subscription"})
			return "invalid"
		}
		subscription := registration.Subscription
		if r.URL.Path == "/push/unsubscribe" {
			if _, err := t.store.DB().ExecContext(r.Context(), "DELETE FROM web_push WHERE endpoint=? AND pubkey=?", subscription.Endpoint, actor); err != nil {
				writeJSON(w, 500, map[string]string{"error": "unsubscribe failed"})
				return "error"
			}
			writeJSON(w, 200, map[string]any{"ok": true})
			return "ok"
		}
		if role, err := t.community.Role(r.Context(), actor); err != nil || (role == "" && actor != t.Policy().Owner) {
			writeJSON(w, 403, map[string]string{"error": "restricted: notifications are limited to relay members"})
			return "unauthorized"
		}
		if err := t.savePushSubscription(r.Context(), actor, registration); err != nil {
			writeJSON(w, statusForPush(err), map[string]string{"error": err.Error()})
			if statusForPush(err) == 400 {
				return "invalid"
			}
			return "error"
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return "ok"
	default:
		http.NotFound(w, r)
		return "invalid"
	}
}

func statusForPush(err error) int {
	if strings.HasPrefix(err.Error(), "invalid:") {
		return 400
	}
	return 500
}

// parsePushRegistration accepts the browser's registration, with or without
// categories, and validates the subscription inside it.
func parsePushRegistration(body []byte) (pushRegistration, error) {
	var registration pushRegistration
	if err := json.Unmarshal(body, &registration); err != nil || registration.Subscription.Endpoint == "" {
		registration = pushRegistration{}
		if err := json.Unmarshal(body, &registration.Subscription); err != nil {
			return registration, err
		}
	}
	if err := registration.Subscription.Validate(); err != nil {
		return registration, err
	}
	var kept []string
	for _, category := range registration.Categories {
		if containsString(pushCategories, category) && !containsString(kept, category) {
			kept = append(kept, category)
		}
	}
	registration.Categories = kept
	return registration, nil
}

func (t *Tenant) savePushSubscription(ctx context.Context, actor string, registration pushRegistration) error {
	subscription := registration.Subscription
	raw, err := json.Marshal(subscription)
	if err != nil {
		return err
	}
	categories := ""
	if len(registration.Categories) > 0 {
		encoded, err := json.Marshal(registration.Categories)
		if err != nil {
			return err
		}
		categories = string(encoded)
	}
	return t.store.WithTx(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM web_push WHERE pubkey=? AND endpoint<>?", actor, subscription.Endpoint).Scan(&count); err != nil {
			return err
		}
		if count >= pushMaxPerPubkey {
			return fmt.Errorf("invalid: at most %d devices per key", pushMaxPerPubkey)
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO web_push(endpoint,pubkey,subscription,created_at,failures,categories) VALUES(?,?,?,?,0,?) ON CONFLICT(endpoint) DO UPDATE SET pubkey=excluded.pubkey, subscription=excluded.subscription, failures=0, categories=excluded.categories", subscription.Endpoint, actor, string(raw), time.Now().Unix(), categories)
		return err
	})
}

// enqueuePush records one notification summary for device delivery. It is
// suitable for records.Config.PushNotification and does nothing for
// recipients without registered devices.
func (t *Tenant) enqueuePush(ctx context.Context, recipient, kind, subject, text string) error {
	return t.enqueuePushPayload(ctx, pushPayload{Recipient: recipient, Kind: kind, Subject: subject, Text: text})
}

func (t *Tenant) enqueuePushPayload(ctx context.Context, payload pushPayload) error {
	var count int
	if err := t.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM web_push WHERE pubkey=?", payload.Recipient).Scan(&count); err != nil || count == 0 {
		return err
	}
	if len(payload.Text) > pushSummaryMaxLength {
		payload.Text = payload.Text[:pushSummaryMaxLength]
	}
	recipient := payload.Recipient
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	payloadJSON := string(encoded)
	return t.store.WithTx(ctx, func(tx *sql.Tx) error {
		return storage.AddIntents(ctx, tx, []storage.Intent{{Kind: notificationPush, EventID: uuid.NewString(), Target: recipient, Payload: payloadJSON}}, time.Now().Unix())
	})
}

func (t *Tenant) handleNotificationPush(ctx context.Context, intent work.Intent) error {
	var payload pushPayload
	if err := json.Unmarshal([]byte(intent.Payload), &payload); err != nil || payload.Recipient != intent.Target {
		return errors.New("notification-push: invalid payload")
	}
	rows, err := t.store.DB().QueryContext(ctx, "SELECT endpoint, subscription, failures, categories FROM web_push WHERE pubkey=?", payload.Recipient)
	if err != nil {
		return err
	}
	type device struct {
		endpoint     string
		subscription webpush.Subscription
		failures     int
	}
	category := payload.Kind
	if !containsString(pushCategories, category) {
		category = pushRelay
	}
	var devices []device
	for rows.Next() {
		var item device
		var raw, categories string
		if err := rows.Scan(&item.endpoint, &raw, &item.failures, &categories); err != nil {
			rows.Close()
			return err
		}
		// A device that chose categories only wakes for those.
		if allowed := decodeCategories(categories); allowed != nil && !allowed[category] {
			continue
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
	message, err := json.Marshal(t.pushMessage(ctx, payload))
	if err != nil {
		return err
	}
	client := t.pushClient
	if client == nil {
		client = replication.NewPinnedClient(10 * time.Second)
	}
	var result error
	delivered, removed, failed := 0, 0, 0
	for _, item := range devices {
		sent, sendErr := webpush.Send(ctx, client, keys, t.pushSubject(), item.subscription, message, pushTTL)
		switch {
		case sendErr == nil:
			delivered++
			_, _ = t.store.DB().ExecContext(ctx, "UPDATE web_push SET failures=0 WHERE endpoint=?", item.endpoint)
		case sent.Gone || item.failures+1 >= pushMaxFailures:
			removed++
			_, _ = t.store.DB().ExecContext(ctx, "DELETE FROM web_push WHERE endpoint=?", item.endpoint)
		default:
			failed++
			_, _ = t.store.DB().ExecContext(ctx, "UPDATE web_push SET failures=failures+1 WHERE endpoint=?", item.endpoint)
			result = errors.Join(result, sendErr)
		}
	}
	// Counts only: endpoints and keys stay out of logs, as they do for metrics.
	t.app.telemetry.Logger().Info("device notifications sent", "tenant", t.meta.Name, "category", category, "delivered", delivered, "failed", failed, "removed", removed)
	return result
}

// pushMessage turns a notification summary into what the device shows.
func (t *Tenant) pushMessage(ctx context.Context, payload pushPayload) pushMessage {
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
	url := payload.URL
	if url == "" {
		url = strings.TrimRight(t.publicURL, "/") + "/inbox"
	}
	return pushMessage{Title: title, Body: body, URL: url, Tag: "tiny-" + payload.Kind, Badge: t.inboxUnread(ctx, payload.Recipient)}
}

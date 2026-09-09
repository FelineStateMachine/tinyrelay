package daemon

// Callback delivery. Each matching event is one POST to the callback URL,
// signed with the callback's secret. A failed POST is tried again after one
// minute and then after five; after 20 failures in a row the callback is
// paused until its owner resumes it.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/work"
)

// callbackPayload is the work intent body: the event itself, so delivery
// does not depend on the event still being stored, and the attempt number.
type callbackPayload struct {
	Event   event.Event `json:"event"`
	Attempt int         `json:"attempt"`
}

// callbackSignature is the X-Tiny-Signature value for a body: sha256= and
// the hex HMAC-SHA256 of the body under the callback secret.
func callbackSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// callbackHTTPClient refuses private addresses and redirects unless the
// relay itself runs on a loopback address.
func (t *Tenant) callbackHTTPClient() *http.Client {
	if t.callbackClient != nil {
		return t.callbackClient
	}
	if t.loopbackRelay() {
		return &http.Client{Timeout: callbackTimeout, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return replication.NewPinnedClient(callbackTimeout)
}

// callbackLock bounds delivery to one POST per callback at a time.
func (t *Tenant) callbackLock(id string) *sync.Mutex {
	t.callbackMu.Lock()
	defer t.callbackMu.Unlock()
	if t.callbackLocks == nil {
		t.callbackLocks = map[string]*sync.Mutex{}
	}
	lock := t.callbackLocks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		t.callbackLocks[id] = lock
	}
	return lock
}

// handleCallbackDelivery serves one callback-delivery intent. It rechecks
// the callback and the key's access right before the POST, so a callback
// removed or paused after the event arrived delivers nothing.
func (t *Tenant) handleCallbackDelivery(ctx context.Context, intent work.Intent) (err error) {
	ctx, finish := t.app.telemetry.Start(ctx, "callback")
	outcome := "error"
	defer func() { finish(outcome) }()
	var payload callbackPayload
	if err := json.Unmarshal([]byte(intent.Payload), &payload); err != nil || payload.Event.ID == "" {
		outcome = "invalid"
		return errors.New("callback-delivery: invalid payload")
	}
	if payload.Attempt < 1 {
		payload.Attempt = 1
	}
	lock := t.callbackLock(intent.Target)
	lock.Lock()
	defer lock.Unlock()
	record, err := t.callback(ctx, intent.Target)
	if err != nil {
		if strings.HasPrefix(err.Error(), "not found:") {
			outcome = "paused"
			return nil
		}
		return err
	}
	if record.Paused {
		outcome = "paused"
		return nil
	}
	role, err := t.community.Role(ctx, record.Owner)
	if err != nil {
		return err
	}
	if role == "" && record.Owner != t.Policy().Owner {
		// The key lost its standing since it registered; stop until a
		// person resumes the callback.
		outcome = "unauthorized"
		return t.pauseCallback(ctx, record.ID, record.Failures, "paused: the key is no longer a member")
	}
	if !t.gate.CanSee(ctx, payload.Event, relay.Session{PubKeys: []string{record.Owner}, RelayURL: t.RelayURL()}, nil) {
		outcome = "unauthorized"
		return nil
	}
	status, postErr := t.postCallback(ctx, record, payload.Event)
	now := time.Now().Unix()
	if postErr == nil {
		outcome = "ok"
		_, err = t.store.DB().ExecContext(ctx, `UPDATE callbacks SET last_delivery_at=?, last_status='ok', failures=0 WHERE id=?`, now, record.ID)
		t.app.telemetry.Logger().Info("callback delivered", "tenant", t.meta.Name, "attempt", payload.Attempt, "status", status)
		return err
	}
	// Counts only: the reason names the HTTP status or the failure class,
	// never the URL.
	reason := postErr.Error()
	failures := record.Failures + 1
	paused := failures >= callbackPauseFailures
	if paused {
		outcome = "paused"
		if err := t.pauseCallback(ctx, record.ID, failures, fmt.Sprintf("paused after %d failures: %s", failures, reason)); err != nil {
			return err
		}
	} else if _, err := t.store.DB().ExecContext(ctx, `UPDATE callbacks SET failures=?, last_status=? WHERE id=?`, failures, reason, record.ID); err != nil {
		return err
	}
	t.app.telemetry.Logger().Info("callback delivery failed", "tenant", t.meta.Name, "attempt", payload.Attempt, "status", status, "failures", failures, "paused", paused)
	if paused || payload.Attempt >= callbackMaxAttempts {
		return nil
	}
	return t.retryCallback(ctx, intent, payload)
}

// pauseCallback stops deliveries and records the count and reason, then
// drops the index so no new intents are queued for the callback.
func (t *Tenant) pauseCallback(ctx context.Context, id string, failures int, status string) error {
	if _, err := t.store.DB().ExecContext(ctx, `UPDATE callbacks SET paused=1, failures=?, last_status=? WHERE id=?`, failures, status, id); err != nil {
		return err
	}
	t.invalidateCallbacks()
	return nil
}

// retryCallback queues the next attempt after its backoff. Each attempt is
// its own intent so the durable queue's ordering and fencing still apply.
func (t *Tenant) retryCallback(ctx context.Context, intent work.Intent, payload callbackPayload) error {
	delay := callbackBackoff[len(callbackBackoff)-1]
	if payload.Attempt-1 < len(callbackBackoff) {
		delay = callbackBackoff[payload.Attempt-1]
	}
	payload.Attempt++
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	key := sha256.Sum256([]byte(callbackDelivery + "\x00" + payload.Event.ID + "\x00" + intent.Target + "\x00" + fmt.Sprint(payload.Attempt)))
	return t.store.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO work_intents(id,kind,event_id,target,payload,next_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, hex.EncodeToString(key[:]), callbackDelivery, payload.Event.ID+"#"+fmt.Sprint(payload.Attempt), intent.Target, string(encoded), now+int64(delay/time.Second), now, now)
		return err
	})
}

// postCallback sends the event and returns the HTTP status with a bounded
// reason on failure.
func (t *Tenant) postCallback(ctx context.Context, record callbackRecord, e event.Event) (int, error) {
	body, err := json.Marshal(e)
	if err != nil {
		return 0, errors.New("encode event")
	}
	ctx, cancel := context.WithTimeout(ctx, callbackTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, record.URL, strings.NewReader(string(body)))
	if err != nil {
		return 0, errors.New("bad url")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Tiny-Callback", record.ID)
	request.Header.Set("X-Tiny-Signature", callbackSignature(record.secret, body))
	request.Header.Set("X-Tiny-Relay", t.publicURL)
	request.Header.Set("User-Agent", "tinyrelay")
	response, err := t.callbackHTTPClient().Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return 0, errors.New("timeout")
		}
		return 0, errors.New("connection failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return response.StatusCode, nil
}

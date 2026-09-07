package daemon

// Notification delivery is deliberately separate from ordinary replication.
// A relay-generated gift wrap is private data: it is stored locally for audit,
// broadcast to current local subscribers through a fenced work item, and sent
// only to the recipient's NIP-17 inbox relay list (kind 10050).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/work"
)

const (
	notificationBroadcast = "notification-broadcast"
	notificationDelivery  = "notification-delivery"
)

type notificationPayload struct {
	Recipient string `json:"recipient"`
}

// deliverNotification is suitable for records.Config.DeliverNotification.
// Save persists the wrap and both fenced intents in one transaction. It does
// not synchronously fan out while the relay publish fence is held.
func (t *Tenant) deliverNotification(ctx context.Context, wrap event.Event, recipient string) error {
	if wrap.Kind != event.KIND_WRAP || recipient == "" || event.Tag(wrap, "p") != recipient {
		return errors.New("notification: invalid recipient or gift wrap")
	}
	payload, err := json.Marshal(notificationPayload{Recipient: recipient})
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	_, err = t.store.Save(ctx, wrap, storage.SaveOptions{
		Now:        now,
		SearchMode: "off",
		Intents: []storage.Intent{
			{Kind: notificationBroadcast, EventID: wrap.ID, Target: t.meta.ID, Payload: string(payload)},
			{Kind: notificationDelivery, EventID: wrap.ID, Target: recipient, Payload: string(payload)},
		},
	})
	return err
}

// notificationHandlers returns handlers to merge into the tenant worker's
// ordinary handler map. The broadcast handler is local-only; the delivery
// handler uses the recipient's kind-10050 relay list and never ordinary NIP-65
// read/write discovery or the tenant's replication outbox.
func (t *Tenant) notificationHandlers() map[string]work.Handler {
	transport := &replication.NostrTransport{Dialer: replication.WebsocketDialer{AllowPrivate: t.app != nil && t.app.cfg.AllowPrivateRelays, MaxMessageBytes: func() int64 {
		if t.app != nil {
			return t.app.cfg.MaxMessageBytes
		}
		return 0
	}()}}
	return map[string]work.Handler{
		notificationBroadcast: t.handleNotificationBroadcast,
		notificationDelivery: func(ctx context.Context, intent work.Intent) error {
			return t.handleNotificationDelivery(ctx, intent, transport)
		},
	}
}

func (t *Tenant) loadNotification(ctx context.Context, id string) (event.Event, error) {
	result, err := t.store.Query(ctx, event.Filter{IDs: []string{id}, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
	if err != nil {
		return event.Event{}, err
	}
	if len(result.Events) == 0 {
		return event.Event{}, nil
	}
	return result.Events[0], nil
}

func (t *Tenant) handleNotificationBroadcast(ctx context.Context, intent work.Intent) error {
	e, err := t.loadNotification(ctx, intent.EventID)
	if err != nil || e.ID == "" {
		return err
	}
	if e.Kind != event.KIND_WRAP || event.Tag(e, "p") == "" {
		return errors.New("notification: stored event is not a recipient gift wrap")
	}
	// BroadcastGenerated is already outside Router.Publish's write fence.
	t.router.BroadcastGenerated(e)
	return nil
}

func (t *Tenant) handleNotificationDelivery(ctx context.Context, intent work.Intent, transport *replication.NostrTransport) error {
	var payload notificationPayload
	if intent.Payload != "" {
		if err := json.Unmarshal([]byte(intent.Payload), &payload); err != nil {
			return fmt.Errorf("notification: payload: %w", err)
		}
	}
	recipient := intent.Target
	if payload.Recipient != "" && payload.Recipient != recipient {
		return errors.New("notification: recipient target mismatch")
	}
	e, err := t.loadNotification(ctx, intent.EventID)
	if err != nil {
		return err
	}
	if e.ID == "" {
		return nil
	}
	if e.Kind != event.KIND_WRAP || event.Tag(e, "p") != recipient {
		return errors.New("notification: recipient check failed")
	}
	targets, lookupErr := t.notificationInboxRelays(ctx, recipient)
	if lookupErr != nil {
		return lookupErr
	}
	if len(targets) == 0 {
		return errors.New("notification: recipient has no kind-10050 inbox relays")
	}
	for _, target := range targets {
		if sameRelay(target, t.RelayURL()) {
			continue
		}
		result, sendErr := transport.Send(ctx, target, e)
		if sendErr != nil {
			return sendErr
		}
		if result.Accepted || strings.HasPrefix(result.Message, "duplicate:") {
			continue
		}
		if result.Message == "" {
			result.Message = "recipient relay rejected notification"
		}
		return errors.New(result.Message)
	}
	return nil
}

func (t *Tenant) notificationInboxRelays(ctx context.Context, recipient string) ([]string, error) {
	result, err := t.store.Query(ctx, event.Filter{Kinds: []int{10050}, Authors: []string{recipient}, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(result.Events) == 0 && t.app != nil {
		dialer := replication.WebsocketDialer{AllowPrivate: t.app.cfg.AllowPrivateRelays, MaxMessageBytes: t.app.cfg.MaxMessageBytes}
		transport := &replication.NostrTransport{Dialer: dialer}
		var newest event.Event
		var queryErr error
		limit := 20
		for _, seed := range t.app.cfg.DiscoveryRelays {
			if !validRelayURL(seed) || sameRelay(seed, t.RelayURL()) {
				continue
			}
			events, qerr := transport.Query(ctx, seed, event.Filter{Kinds: []int{10050}, Authors: []string{recipient}, Tags: map[string][]string{}, Limit: &limit})
			if qerr != nil {
				queryErr = qerr
				continue
			}
			for _, candidate := range events {
				if candidate.Kind == 10050 && candidate.PubKey == recipient && (newest.ID == "" || candidate.CreatedAt > newest.CreatedAt) {
					newest = candidate
				}
			}
		}
		if newest.ID != "" {
			if _, saveErr := t.store.Save(ctx, newest, storage.SaveOptions{Now: time.Now().Unix(), SearchMode: "off"}); saveErr != nil && !errors.Is(saveErr, storage.ErrDuplicate) {
				return nil, saveErr
			}
			result.Events = []event.Event{newest}
		} else if queryErr != nil {
			return nil, fmt.Errorf("notification: inbox discovery: %w", queryErr)
		}
	}
	if len(result.Events) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, tag := range result.Events[0].Tags {
		if len(tag) < 2 || tag[0] != "relay" || !validRelayURL(tag[1]) || seen[tag[1]] {
			continue
		}
		seen[tag[1]] = true
		out = append(out, tag[1])
	}
	return out, nil
}

func validRelayURL(raw string) bool {
	return strings.HasPrefix(raw, "ws://") || strings.HasPrefix(raw, "wss://")
}

func sameRelay(a, b string) bool { return strings.TrimRight(a, "/") == strings.TrimRight(b, "/") }

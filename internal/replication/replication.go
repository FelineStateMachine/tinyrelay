// Package replication contains durable self-hosted relay fanout and job
// planning. Network I/O is deliberately behind small interfaces so a relay
// can use local test transports, a normal websocket client, or an operator's
// proxy without changing queue semantics.
package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type Origin string

const (
	OriginClient        Origin = "client"
	OriginImport        Origin = "import"
	OriginServer        Origin = "server"
	OriginPush          Origin = "push"
	OriginBackupRestore Origin = "backup_restore"
)

// Policy contains only routing decisions. It has no lease, fuel, or product
// quota fields; the host controls process capacity independently.
type Policy struct {
	Enabled         bool
	ReadMembersOnly bool
	SelfPubKey      string
	SelfRelayURL    string
}

type RelayDirectory interface {
	WriteRelays(pubkey string) []string
	ReadRelays(pubkey string) []string
}

// RelayListDiscovery preserves the source order of all NIP-65 relay tags for
// owner backfill. RelayDirectory's directional methods remain sufficient for
// delivery planning.
type RelayListDiscovery interface {
	RelayList(pubkey string) []string
}

type Discovery interface {
	DiscoverRelays(ctx context.Context, pubkey string) ([]string, error)
}

type ReadDiscovery interface {
	DiscoverReadRelays(ctx context.Context, pubkey string) ([]string, error)
}

type IntentPayload struct {
	Origin Origin `json:"origin"`
}

// PrepareIntents computes NIP-65 outbox work. The event transaction should
// pass the returned references to storage.Save as SaveOptions.Intents.
func PrepareIntents(ctx context.Context, e event.Event, origin Origin, policy Policy, directory RelayDirectory) []storage.Intent {
	if err := contextErr(ctx); err != nil || !eligible(e, origin, policy) {
		return nil
	}
	targets := uniqueTargets(directory.WriteRelays(e.PubKey))
	for _, pubkey := range event.TagValues(e, "p") {
		targets = appendUnique(targets, directory.ReadRelays(pubkey))
	}
	payload, err := json.Marshal(IntentPayload{Origin: origin})
	if err != nil {
		return nil
	}
	intents := make([]storage.Intent, 0, len(targets))
	for _, target := range targets {
		if !safeRelayURL(target) {
			continue
		}
		if policy.SelfRelayURL != "" && canonicalRelayURL(target) == canonicalRelayURL(policy.SelfRelayURL) {
			continue
		}
		intents = append(intents, storage.Intent{Kind: "delivery", EventID: e.ID, Target: target, Payload: string(payload)})
	}
	return intents
}

func contextErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func eligible(e event.Event, origin Origin, policy Policy) bool {
	peerPublication := origin == OriginServer && e.Kind == event.KIND_RELAY_DISCOVERY && e.PubKey == policy.SelfPubKey
	if !policy.Enabled || policy.ReadMembersOnly || (origin != OriginClient && !peerPublication) || (e.PubKey == policy.SelfPubKey && !peerPublication) || privateKind(e.Kind) {
		return false
	}
	for _, tag := range e.Tags {
		if len(tag) > 0 && tag[0] == "-" {
			return false
		}
	}
	return true
}

func privateKind(kind int) bool {
	return event.IsPrivate(kind) || kind == event.KIND_DM
}

func uniqueTargets(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if safeRelayURL(value) {
			out = appendUnique(out, []string{value})
		}
	}
	return out
}

func appendUnique(out []string, values []string) []string {
	for _, value := range values {
		value = strings.TrimRight(strings.TrimSpace(value), "/")
		if value == "" || contains(out, value) {
			continue
		}
		out = append(out, value)
	}
	return out
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func safeRelayURL(raw string) bool {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	return true
}

func canonicalRelayURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimRight(strings.TrimSpace(raw), "/")
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	}
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" {
		host += ":" + port
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	if path == "" {
		path = "/"
	}
	return scheme + "://" + host + path + "?" + u.RawQuery
}

type DeliveryTransport interface {
	Send(ctx context.Context, target string, e event.Event) (DeliveryResult, error)
}

type DeliveryResult struct {
	Accepted bool
	Message  string
}

func ValidateIntent(intent storage.Intent) error {
	if intent.Kind == "" || intent.EventID == "" || !safeRelayURL(intent.Target) {
		return errors.New("replication: malformed intent")
	}
	return nil
}

func DecodeIntentPayload(raw string) (IntentPayload, error) {
	var payload IntentPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return payload, fmt.Errorf("replication: decode intent payload: %w", err)
	}
	return payload, nil
}

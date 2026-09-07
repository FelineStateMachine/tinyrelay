package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

func TestCapabilityRegistryReportsHonestOptionalFeatures(t *testing.T) {
	monitor, err := NewPeerMonitor(PeerMonitorConfig{Peers: []string{"https://peer.example"}, Publish: false})
	if err != nil {
		t.Fatal(err)
	}
	capabilities := CapabilityRegistryWithServices(policy.Policy{Features: policy.Features{Grasp: true}}, true, true, monitor)
	got := map[string]Capability{}
	for _, capability := range capabilities {
		got[capability.ID] = capability
	}
	if got["NIP-34"].Status != "enabled" || got["GRASP"].Status != "enabled" {
		t.Fatalf("Git capabilities = %#v", got)
	}
	if got["BUD-01"].Status != "enabled" || got["BUD-06"].Status != "enabled" || got["NIP-66"].Status != "configured" {
		t.Fatalf("optional capabilities = %#v", got)
	}
	if got["NIP-94"].Status != "enabled" || got["NIP-96"].Status != "enabled" {
		t.Fatalf("file capabilities = %#v", got)
	}
}

func TestPublishPeerStatusStoresSignedDiscoveryEvent(t *testing.T) {
	_, tenant := testTenant(t)
	if err := tenant.PublishPeerStatus(context.Background(), []PeerStatus{{URL: "https://peer.example", Up: true}}); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := tenant.store.DB().QueryRow(`SELECT raw FROM events WHERE kind=? ORDER BY seq DESC LIMIT 1`, 30166).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var stored struct {
		ID     string `json:"id"`
		PubKey string `json:"pubkey"`
		Sig    string `json:"sig"`
	}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.ID == "" || stored.PubKey == "" || stored.Sig == "" {
		t.Fatalf("discovery event is not signed: %#v", stored)
	}
	if stored.PubKey != tenant.records.PublicKey() {
		t.Fatalf("publisher = %q, relay identity = %q", stored.PubKey, tenant.records.PublicKey())
	}
}

func TestCapabilityRegistryDisablesUnmountedBlobDoors(t *testing.T) {
	for _, capability := range CapabilityRegistry(policy.Policy{}, false, nil) {
		if len(capability.ID) > 4 && capability.ID[:4] == "BUD-" && capability.Status == "enabled" {
			t.Fatalf("unmounted blob capability advertised: %#v", capability)
		}
	}
}

func TestPeerMonitorProbesOnlyConfiguredPeers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/nostr+json" {
			t.Errorf("accept = %q", r.Header.Get("Accept"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	var outcomes []string
	monitor, err := NewPeerMonitor(PeerMonitorConfig{Peers: []string{server.URL}, Timeout: time.Second, Observe: func(outcome string, _ time.Duration) { outcomes = append(outcomes, outcome) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := monitor.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	peers := monitor.Peers()
	if len(peers) != 1 || peers[0] != server.URL {
		t.Fatalf("peers = %#v", peers)
	}
	snapshot := monitor.Snapshot()
	if len(snapshot) != 1 || !snapshot[0].Up || snapshot[0].LastSuccess.IsZero() || len(outcomes) != 1 || outcomes[0] != "ok" {
		t.Fatalf("snapshot = %#v outcomes = %#v", snapshot, outcomes)
	}
}

func TestPeerMonitorRejectsUnboundedConfiguration(t *testing.T) {
	if _, err := NewPeerMonitor(PeerMonitorConfig{Peers: []string{"wss://public.example", "not a URL"}}); err == nil {
		t.Fatal("invalid peer configuration accepted")
	}
}

func TestPeerMonitorPublicationRequiresPublisher(t *testing.T) {
	withoutPublisher, err := NewPeerMonitor(PeerMonitorConfig{Peers: []string{"https://peer.example"}, Publish: true})
	if err != nil {
		t.Fatal(err)
	}
	if withoutPublisher.PublicationEnabled() {
		t.Fatal("publication marked enabled without publisher")
	}
	withPublisher, err := NewPeerMonitor(PeerMonitorConfig{Peers: []string{"https://peer.example"}, Publish: true, PublishFunc: func(context.Context, []PeerStatus) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if !withPublisher.PublicationEnabled() {
		t.Fatal("configured publisher not recognized")
	}
}

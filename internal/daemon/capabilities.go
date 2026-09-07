package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// Capability describes an implemented protocol surface and its operational
// status. Status is intentionally explicit so NIP-11 or an operator UI does
// not advertise a feature merely because a name appears in a registry.
type Capability struct {
	ID       string   `json:"id"`
	Status   string   `json:"status"`
	Reason   string   `json:"reason,omitempty"`
	Profiles []string `json:"profiles,omitempty"`
}

func capabilityNIPNumber(id string) (int, bool) {
	if !strings.HasPrefix(id, "NIP-") {
		return 0, false
	}
	number, err := strconv.Atoi(strings.TrimPrefix(id, "NIP-"))
	return number, err == nil
}

// Capabilities returns the capability registry for a tenant. Optional
// operator peer liveness is passed explicitly and is never inferred by
// crawling public relays.
func (t *Tenant) Capabilities(peers *PeerMonitor) []Capability {
	if t == nil {
		return nil
	}
	p := t.Policy()
	capabilities := CapabilityRegistryWithServices(p, t.git != nil, t.blobs != nil && p.Features.Files, peers)
	if t.community != nil {
		setCapability(capabilities, "NIP-43", Capability{ID: "NIP-43", Status: "enabled", Reason: "membership protocol"})
		setCapability(capabilities, "NIP-56", Capability{ID: "NIP-56", Status: "enabled", Reason: "moderation reports"})
	}
	if t.blobs != nil && p.Features.Files {
		setCapability(capabilities, "NIP-94", Capability{ID: "NIP-94", Status: "enabled", Reason: "file metadata descriptors"})
		setCapability(capabilities, "NIP-96", Capability{ID: "NIP-96", Status: "enabled", Reason: "HTTP file storage"})
	}
	for index := range capabilities {
		if capabilities[index].ID == "GRASP" && t.git != nil {
			capabilities[index].Profiles = t.git.SupportedGRASPs()
		}
	}
	return capabilities
}

func CapabilityRegistry(p policy.Policy, gitAvailable bool, peers *PeerMonitor) []Capability {
	return CapabilityRegistryWithServices(p, gitAvailable, false, peers)
}

func CapabilityRegistryWithServices(p policy.Policy, gitAvailable, blobsAvailable bool, peers *PeerMonitor) []Capability {
	capabilities := []Capability{
		{ID: "NIP-01", Status: "enabled", Reason: "relay protocol"},
		{ID: "NIP-09", Status: "enabled", Reason: "event deletion"},
		{ID: "NIP-11", Status: "enabled", Reason: "relay information document"},
		{ID: "NIP-13", Status: "enabled", Reason: "proof of work policy"},
		{ID: "NIP-17", Status: "enabled", Reason: "private direct messages"},
		{ID: "NIP-40", Status: "enabled", Reason: "expiration tags"},
		{ID: "NIP-42", Status: "enabled", Reason: "authenticated relay sessions"},
		{ID: "NIP-62", Status: "enabled", Reason: "request to vanish"},
		{ID: "NIP-67", Status: "enabled", Reason: "EOSE completeness hints"},
		{ID: "NIP-70", Status: "enabled", Reason: "protected event policy"},
		{ID: "NIP-86", Status: "enabled", Reason: "operator management methods"},
		{ID: "NIP-98", Status: "enabled", Reason: "HTTP authentication"},
		{ID: "NIP-43", Status: "disabled", Reason: "membership service is not mounted"},
		{ID: "NIP-56", Status: "disabled", Reason: "moderation report service is not mounted"},
		{ID: "NIP-94", Status: "disabled", Reason: "file service is not mounted"},
		{ID: "NIP-96", Status: "disabled", Reason: "file service is not mounted"},
		{ID: "NIP-34", Status: "disabled", Reason: "Git repository support is not enabled"},
		{ID: "GRASP", Status: "disabled", Reason: "GRASP is not enabled"},
		{ID: "BUD-01", Status: "enabled", Reason: "blob retrieval and basic server behavior"},
		{ID: "BUD-02", Status: "enabled", Reason: "blob upload and management"},
		{ID: "BUD-04", Status: "enabled", Reason: "blob mirroring"},
		{ID: "BUD-06", Status: "enabled", Reason: "blob existence and metadata checks"},
		{ID: "BUD-09", Status: "enabled", Reason: "blob reports"},
		{ID: "BUD-11", Status: "enabled", Reason: "Nostr authorization"},
		{ID: "BUD-12", Status: "enabled", Reason: "cursor-paginated blob listing"},
		{ID: "NIP-66", Status: "disabled", Reason: "operator peer liveness is not configured"},
	}
	if p.Features.Count {
		capabilities = append(capabilities, Capability{ID: "NIP-45", Status: "enabled", Reason: "count queries"})
	}
	if p.Features.Search != "off" {
		capabilities = append(capabilities, Capability{ID: "NIP-50", Status: "enabled", Reason: "search mode " + p.Features.Search})
	}
	if p.Features.Sync {
		capabilities = append(capabilities, Capability{ID: "NIP-77", Status: "enabled", Reason: "negentropy synchronization"})
	}
	if p.Features.Names {
		capabilities = append(capabilities, Capability{ID: "NIP-05", Status: "enabled", Reason: "name resolution"})
	}
	if p.Features.Signer {
		capabilities = append(capabilities, Capability{ID: "NIP-46", Status: "enabled", Reason: "remote signer"})
	}
	if gitAvailable && p.Features.Grasp {
		setCapability(capabilities, "NIP-34", Capability{ID: "NIP-34", Status: "enabled", Reason: "Git repository support"})
		profiles := []string{"GRASP-01"}
		if p.Features.Grasp06 {
			profiles = append(profiles, "GRASP-06")
		}
		setCapability(capabilities, "GRASP", Capability{ID: "GRASP", Status: "enabled", Reason: "configured GRASP profiles", Profiles: profiles})
	} else if gitAvailable {
		setCapability(capabilities, "NIP-34", Capability{ID: "NIP-34", Status: "configured", Reason: "Git support is available; GRASP policy is disabled"})
	}
	if peers != nil {
		status := "configured"
		reason := "configured peers are monitored; signed publication is not configured"
		if peers.PublicationEnabled() {
			status, reason = "enabled", "configured peer liveness publication is enabled"
		}
		setCapability(capabilities, "NIP-66", Capability{ID: "NIP-66", Status: status, Reason: reason})
	}
	if !blobsAvailable {
		for index := range capabilities {
			if strings.HasPrefix(capabilities[index].ID, "BUD-") {
				capabilities[index].Status = "disabled"
				capabilities[index].Reason = "file service is not mounted"
			}
		}
	} else {
		setCapability(capabilities, "NIP-94", Capability{ID: "NIP-94", Status: "enabled", Reason: "file metadata descriptors"})
		setCapability(capabilities, "NIP-96", Capability{ID: "NIP-96", Status: "enabled", Reason: "HTTP file storage"})
	}
	return capabilities
}

// PublishPeerStatus creates the relay's signed NIP-66 discovery record and
// stores it locally. Delivery is delegated to the normal durable outbox when
// the tenant has delivery enabled; no peer is contacted by this method.
func (t *Tenant) PublishPeerStatus(ctx context.Context, statuses []PeerStatus) error {
	if t == nil || t.records == nil {
		return errors.New("daemon: tenant records are not ready")
	}
	content, err := json.Marshal(map[string]any{"relay": t.RelayURL(), "peers": statuses})
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	e, err := t.records.GenerateEphemeral(ctx, event.KIND_RELAY_DISCOVERY, [][]string{{"d", t.RelayURL()}}, string(content), now)
	if err != nil {
		return err
	}
	return t.generatedRelayRecord(ctx, e)
}

func (t *Tenant) generatedRelayRecord(ctx context.Context, e event.Event) error {
	ctx, done, err := t.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer done()
	_, err = t.router.CommitGenerated(ctx, func(ctx context.Context) (event.Event, error) {
		opts := storage.SaveOptions{Now: e.CreatedAt, SearchMode: t.Policy().Features.Search}
		if t.replication != nil {
			opts.Intents = t.replication.Prepare(e, replication.OriginServer)
		}
		_, saveErr := t.store.Save(ctx, e, opts)
		return e, saveErr
	})
	return err
}

func setCapability(capabilities []Capability, id string, value Capability) {
	for index := range capabilities {
		if capabilities[index].ID == id {
			capabilities[index] = value
			return
		}
	}
}

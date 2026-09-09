package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestBrowseProfileReturnsTheHeldProfileAndWriteRelays(t *testing.T) {
	h := newRoomHarness(t)
	h.must("owner", event.KIND_PROFILE, nil, `{"name":"dami","about":"relay keeper","lud16":"dami@wallet.example"}`)
	h.must("owner", 10002, [][]string{{"r", "wss://relay.one/"}, {"r", "wss://read.only", "read"}, {"r", "wss://relay.two", "write"}}, "")
	result, err := h.tenant.Execute(h.ctx, h.keys["owner"], "browseprofile", []json.RawMessage{rawJSON(map[string]any{})})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	var got struct {
		Pubkey  string         `json:"pubkey"`
		Profile map[string]any `json:"profile"`
		Relays  []string       `json:"relays"`
		Event   *event.Event   `json:"event"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Pubkey != h.keys["owner"] || got.Profile["name"] != "dami" || got.Profile["lud16"] != "dami@wallet.example" || got.Event == nil || got.Event.Kind != 0 {
		t.Fatalf("own profile = %s", raw)
	}
	if strings.Join(got.Relays, " ") != "wss://relay.one/ wss://relay.two" {
		t.Fatalf("write relays = %v", got.Relays)
	}
	// Another member reads the owner's profile by key; an unknown key is empty.
	result, err = h.tenant.Execute(h.ctx, h.keys["alice"], "browseprofile", []json.RawMessage{rawJSON(map[string]any{"pubkey": h.keys["owner"]})})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(result)
	if !strings.Contains(string(raw), `"name":"dami"`) {
		t.Fatalf("profile by key = %s", raw)
	}
	result, err = h.tenant.Execute(h.ctx, h.keys["alice"], "browseprofile", []json.RawMessage{rawJSON(map[string]any{"pubkey": strings.Repeat("e", 64)})})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(result)
	if !strings.Contains(string(raw), `"event":null`) || !strings.Contains(string(raw), `"profile":{}`) {
		t.Fatalf("unknown profile = %s", raw)
	}
	if _, err := h.tenant.Execute(h.ctx, h.keys["alice"], "browseprofile", []json.RawMessage{rawJSON(map[string]any{"pubkey": "nope"})}); err == nil || !strings.HasPrefix(err.Error(), "invalid:") {
		t.Fatalf("bad key: %v", err)
	}
}

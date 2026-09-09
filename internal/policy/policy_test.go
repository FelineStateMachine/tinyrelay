package policy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestPatchOmittedAndEmptySections(t *testing.T) {
	cur := Defaults("owner")
	cur.Tags = []string{"one"}
	cur.Features.Files = true
	got, err := Patch(cur, map[string]json.RawMessage{
		"tags":     json.RawMessage(`[]`),
		"features": json.RawMessage(`{"search":"off"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tags) != 0 || got.Features.Search != "off" || !got.Features.Files {
		t.Fatalf("patch did not preserve or clear fields: %#v", got)
	}
}

func TestJobsDefaultOnUntilSwitchedOff(t *testing.T) {
	if !Defaults("owner").Features.Jobs {
		t.Fatal("long tasks are off by default")
	}
	var stored Policy
	if err := json.Unmarshal([]byte(`{"owner":"owner","writes":"open","reads":"open","features":{"search":"prose"}}`), &stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Features.Jobs {
		t.Fatal("a stored policy without the field lost long tasks")
	}
	patched, err := Patch(Defaults("owner"), map[string]json.RawMessage{"features": json.RawMessage(`{"jobs":false}`)})
	if err != nil {
		t.Fatal(err)
	}
	if patched.Features.Jobs || patched.Features.Search != "prose" {
		t.Fatalf("patch did not switch long tasks off: %#v", patched.Features)
	}
}

func TestPrivateReadRequiresRecipient(t *testing.T) {
	p := Defaults("owner")
	p.Reads = "members"
	e := event.Event{Kind: 1059, Tags: [][]string{{"p", "recipient"}}}
	if CanRead(p, e, Access{Member: true, PubKeys: []string{"other"}}) {
		t.Fatal("member without recipient identity can read private event")
	}
	if !CanRead(p, e, Access{Member: true, PubKeys: []string{"recipient"}}) {
		t.Fatal("recipient cannot read private event")
	}
}

func TestPolicyJSONHasNoHostedEconomy(t *testing.T) {
	b, err := json.Marshal(Defaults(strings.Repeat("a", 64)))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"lease", "fuel", "maxBlobMB", "eventsPerMinute", "reqsPerMinute"} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("policy contains hosted field %q: %s", forbidden, b)
		}
	}
}

func TestGRASP08RequiresPrivateMembershipReads(t *testing.T) {
	p := Defaults(strings.Repeat("a", 64))
	p.Features.Grasp08 = true
	p.Features.Grasp = true
	if err := Validate(p); err == nil || !strings.Contains(err.Error(), "reads") {
		t.Fatalf("GRASP-08 with open reads accepted: %v", err)
	}
	p.Reads = "members"
	if err := Validate(p); err != nil {
		t.Fatalf("valid private GRASP-08 policy rejected: %v", err)
	}
}

func TestGRASP08RequiresBaseGRASP(t *testing.T) {
	p := Defaults(strings.Repeat("a", 64))
	p.Features.Grasp08 = true
	p.Reads = "members"
	p.Features.Grasp = false
	if err := Validate(p); err == nil || !strings.Contains(err.Error(), "subfeatures") {
		t.Fatalf("GRASP-08 without base GRASP accepted: %v", err)
	}
}

func TestPrivateRepositoryRequiresGRASP08PrivateTenant(t *testing.T) {
	e := event.Event{Kind: 30617, Tags: [][]string{{"private", "true"}}}
	if CanRead(Policy{Reads: "open"}, e, Access{}) {
		t.Fatal("private repository was readable on an open tenant")
	}
	p := Policy{Reads: "members"}
	if !CanRead(p, event.Event{Kind: 1}, Access{Member: true}) {
		t.Fatal("ordinary event became unreadable on a member tenant")
	}
	p.Features.Grasp08 = true
	if !CanRead(p, e, Access{Member: true}) {
		t.Fatal("private repository was unreadable on a GRASP-08 member tenant")
	}
}

func TestGRASPHistoryExtensionsRequireGRASP02(t *testing.T) {
	cases := []struct {
		name string
		set  func(*Features)
	}{
		{name: "grasp03", set: func(f *Features) { f.Grasp03 = true }},
		{name: "grasp05", set: func(f *Features) { f.Grasp05 = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Defaults(strings.Repeat("a", 64))
			p.Features.Grasp = true
			tc.set(&p.Features)
			if err := Validate(p); err == nil || !strings.Contains(err.Error(), "grasp02") {
				t.Fatalf("accepted %s without GRASP-02: %v", tc.name, err)
			}
			p.Features.Grasp02 = true
			if err := Validate(p); err != nil {
				t.Fatalf("rejected %s with GRASP-02: %v", tc.name, err)
			}
		})
	}
}

func TestValidatePrivatePeers(t *testing.T) {
	p := Defaults(strings.Repeat("a", 64))
	p.PrivatePeers = []string{"wss://peer.example/relay/"}
	if err := Validate(p); err != nil {
		t.Fatal(err)
	}
	p.PrivatePeers = []string{"https://peer.example/?token=bad"}
	if err := Validate(p); err == nil {
		t.Fatal("accepted private peer URL with query credentials")
	}
}

package gitrelay

import (
	"context"
	"reflect"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

func TestCapabilitiesRequireEventSyncTransport(t *testing.T) {
	p := policy.Policy{Features: policy.Features{Grasp: true, Grasp02: true, Grasp03: true, Grasp06: true}}
	g := &GitRelay{policy: func() policy.Policy { return p }}
	if got := g.Capabilities(); !reflect.DeepEqual(got, []string{"GRASP-01", "GRASP-06"}) {
		t.Fatalf("without event transport: %v", got)
	}
	g.eventSync = func(context.Context, Repository) error { return nil }
	if got := g.Capabilities(); !reflect.DeepEqual(got, []string{"GRASP-01", "GRASP-02", "GRASP-03", "GRASP-06"}) {
		t.Fatalf("with event transport: %v", got)
	}
	p.Features.Grasp02 = false
	if got := g.Capabilities(); !reflect.DeepEqual(got, []string{"GRASP-01", "GRASP-06"}) {
		t.Fatalf("without base sync: %v", got)
	}
}

func TestCapabilitiesPreservePrivateProfiles(t *testing.T) {
	p := policy.Policy{Reads: "members", Features: policy.Features{Grasp: true, Grasp02: true, Grasp03: true, Grasp06: true, Grasp08: true}}
	g := &GitRelay{policy: func() policy.Policy { return p }, eventSync: func(context.Context, Repository) error { return nil }}
	want := []string{"GRASP-01", "GRASP-02", "GRASP-03", "GRASP-06", "GRASP-08"}
	if got := g.SupportedGRASPs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("private profiles: %v", got)
	}
}

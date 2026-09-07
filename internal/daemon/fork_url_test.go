package daemon

import (
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/catalog"
)

func TestForkRelayURLUsesProcessBase(t *testing.T) {
	app := &App{cfg: Config{PublicURL: "https://relay.example"}}
	source := &Tenant{meta: structTenant("alice"), publicURL: "https://relay.example/r/alice"}
	if got := app.forkRelayURL(source, "qa"); got != "https://relay.example/r/qa" {
		t.Fatalf("fork URL = %q", got)
	}
}

func TestForkRelayURLStripsNestedTenantPath(t *testing.T) {
	app := &App{}
	source := &Tenant{meta: structTenant("alice"), publicURL: "http://relay.example/r/alice"}
	if got := app.forkRelayURL(source, "qa"); got != "http://relay.example/r/qa" {
		t.Fatalf("fork URL = %q", got)
	}
}

func structTenant(name string) catalog.Tenant { return catalog.Tenant{Name: name} }

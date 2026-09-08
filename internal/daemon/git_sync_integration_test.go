package daemon

import (
	"context"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
)

func TestGitTargetTransportRequiresFeatureAndPrivatePeer(t *testing.T) {
	_, tenant := testTenant(t)
	repo := gitrelay.Repository{Owner: tenant.Policy().Owner, Identifier: "test"}
	if _, err := tenant.gitTargetTransport(context.Background(), repo, "wss://unconfigured.example"); err == nil {
		t.Fatal("disabled sync opened a transport")
	}
	p := tenant.Policy()
	p.Features.Grasp, p.Features.Grasp02, p.Features.Grasp08 = true, true, true
	p.Reads = "members"
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.gitTargetTransport(context.Background(), repo, "wss://unconfigured.example"); err == nil {
		t.Fatal("private repository could send filters to an unconfigured peer")
	}
}

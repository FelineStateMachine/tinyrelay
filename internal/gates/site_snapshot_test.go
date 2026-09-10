package gates

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
)

func validSiteSnapshot(t *testing.T, secret, source string, createdAt int64) event.Event {
	t.Helper()
	hash := strings.Repeat("a", 64)
	paths := [][]string{{"path", "/index.html", hash}}
	e := event.Event{
		Kind:      sites.KindSiteSnapshot,
		CreatedAt: createdAt,
		Tags: append(paths,
			[]string{"x", sites.Aggregate(paths), "aggregate"},
			[]string{"a", "15128:" + source + ":"},
		),
	}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	if err := sites.ValidateManifest(e); err != nil {
		t.Fatalf("snapshot fixture is invalid: %v", err)
	}
	return e
}

func TestSiteSnapshotIsNotControlledByJobFeature(t *testing.T) {
	f := newAgentFixture(t)
	ctx := context.Background()
	snapshot := validSiteSnapshot(t, agentOtherSecret, f.owner, agentNow)
	session := relay.Session{PubKeys: []string{snapshot.PubKey}}

	for _, jobsEnabled := range []bool{true, false} {
		t.Run("jobs-enabled="+strconv.FormatBool(jobsEnabled), func(t *testing.T) {
			p := policy.Defaults(f.owner)
			p.Features.Jobs = jobsEnabled
			f.gate.cfg.Policy = func() policy.Policy { return p }
			if err := f.gate.Write(ctx, snapshot, session, agentNow); err != nil {
				t.Fatalf("valid site snapshot rejected with jobs=%t: %v", jobsEnabled, err)
			}
		})
	}
}

func TestSiteSnapshotRespectsRelayWritePolicy(t *testing.T) {
	f := newAgentFixture(t)
	p := policy.Defaults(f.owner)
	p.Writes = "allowlist"
	p.Features.Jobs = false
	f.gate.cfg.Policy = func() policy.Policy { return p }
	snapshot := validSiteSnapshot(t, agentOtherSecret, f.owner, agentNow)
	err := f.gate.Write(context.Background(), snapshot, relay.Session{PubKeys: []string{snapshot.PubKey}}, agentNow)
	expectError(t, err, "blocked:", "relay write policy")
}

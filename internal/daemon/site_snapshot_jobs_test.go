package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/mcp"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
)

func TestSiteSnapshotStaysOutOfJobReads(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	now := time.Now().Unix()
	owner := tenant.Policy().Owner
	paths := [][]string{{"path", "/index.html", strings.Repeat("a", 64)}}
	tags := append(paths, []string{"x", sites.Aggregate(paths), "aggregate"}, []string{"a", "15128:" + owner + ":"})
	snapshot := wikiPublish(t, tenant, wikiOwnerSecret, event.KIND_SITE_SNAPSHOT, now, "", tags...)
	job := wikiPublish(t, tenant, wikiOwnerSecret, 5001, now-1, "", []string{"i", "hello", "text"})

	result, err := tenant.browseJobs(ctx, owner, jobBrowseRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	items := result.(map[string]any)["items"].([]jobItem)
	if len(items) != 1 || items[0].ID != job.ID {
		t.Fatalf("job listing = %+v; want request %s", items, job.ID)
	}
	if _, err := tenant.browseJob(ctx, owner, snapshot.ID); err == nil || !strings.Contains(err.Error(), "not found: job request") {
		t.Fatalf("snapshot job read error = %v", err)
	}
	if _, err := tenant.browseJob(ctx, owner, job.ID); err != nil {
		t.Fatalf("job read: %v", err)
	}
}

func TestSiteSnapshotCannotBuildJobRequest(t *testing.T) {
	if err := mcp.Validate(mcpJobKind, float64(event.KIND_SITE_SNAPSHOT)); err == nil {
		t.Fatal("site snapshot kind accepted by the advertised job schema")
	}
	if _, err := mcpBuildJobRequest(mcp.Call{Arguments: map[string]any{"kind": float64(event.KIND_SITE_SNAPSHOT)}}); err == nil {
		t.Fatal("site snapshot kind accepted by the job request builder")
	}
	if err := mcpCheckJobRequest(event.Event{Kind: event.KIND_SITE_SNAPSHOT}); err == nil {
		t.Fatal("site snapshot accepted by the signed job request checker")
	}
}

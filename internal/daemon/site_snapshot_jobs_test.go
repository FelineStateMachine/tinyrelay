package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
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
	job := wikiPublish(t, tenant, wikiOwnerSecret, event.KIND_JOB_REQUEST, now-1, "hello", []string{"h", jobTestRoom}, []string{"p", owner})

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

func TestSiteSnapshotIsNotAJobRequest(t *testing.T) {
	if event.IsJobKind(event.KIND_SITE_SNAPSHOT) {
		t.Fatal("site snapshot kind classified as a long task")
	}
	if err := mcpCheckJobRequest(event.Event{Kind: event.KIND_SITE_SNAPSHOT}); err == nil {
		t.Fatal("site snapshot accepted by the signed job request checker")
	}
}

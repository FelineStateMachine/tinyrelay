package replication

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestJobLifecyclePublishesArtifactAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "relay.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	e := signedEvent(t, 100)
	if _, err := store.Save(ctx, e, storage.SaveOptions{Now: 101}); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(Config{Store: store, DataDir: dir, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.AddJob(ctx, JobSpec{ID: "dump-lifecycle", Kind: JobDump, Relays: []string{"events.jsonl"}, Every: 1}); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- service.Run(runCtx) }()
	status := waitJobStatus(t, service, "dump-lifecycle", func(status JobStatus) bool { return status.Phase == "complete" })
	deadline := time.Now().Add(time.Second)
	for {
		pending, listErr := service.Queue().List(ctx, "pending")
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(pending) == 1 && pending[0].EventID == "dump-lifecycle" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recurring run was not enqueued: %#v", pending)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("worker exit = %v", err)
	}
	if status.Started == 0 || status.Finished == 0 {
		t.Fatalf("missing lifecycle timestamps: %#v", status)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("artifact is not JSON: %v", err)
	}
	if decoded["id"] != e.ID {
		t.Fatalf("artifact event id = %v, want %s", decoded["id"], e.ID)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resumed, err := NewService(Config{Store: reopened, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	statuses, err := resumed.ListJobStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Phase != "complete" {
		t.Fatalf("reopened status = %#v", statuses)
	}
}

func TestFailedJobRecordsFailureAndRemainsRetryable(t *testing.T) {
	ctx := context.Background()
	store := openReplicationStore(t)
	service, err := NewService(Config{Store: store, DataDir: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	spec := JobSpec{ID: "missing-pull", Kind: JobPull, Relays: []string{"wss://relay.example"}}
	if err := service.AddJob(ctx, spec); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- service.Run(runCtx) }()
	status := waitJobStatus(t, service, spec.ID, func(status JobStatus) bool { return status.Phase == "failed" })
	if status.Error == "" {
		t.Fatal("failed job did not record an error")
	}
	pending, err := service.Queue().List(ctx, "pending,running")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Attempts < 1 {
		t.Fatalf("retryable intent = %#v", pending)
	}
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("worker exit = %v", err)
	}
}

type integrationPull struct{ events []event.Event }

type ownerReadDiscovery struct{ relays []string }

func (d ownerReadDiscovery) DiscoverRelays(context.Context, string) ([]string, error) {
	return nil, nil
}
func (d ownerReadDiscovery) DiscoverReadRelays(context.Context, string) ([]string, error) {
	return d.relays, nil
}

func TestInboxPullDiscoversOwnerReadRelaysWithoutStaticTargets(t *testing.T) {
	ctx := context.Background()
	store := openReplicationStore(t)
	defer store.Close()
	item := signedEvent(t, 400)
	service, err := NewService(Config{
		Store: store, DataDir: t.TempDir(), Discovery: ownerReadDiscovery{relays: []string{"ws://owner-read.example"}},
		Pull:   integrationPull{events: []event.Event{item}},
		Ingest: func(context.Context, event.Event, Origin) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := JobSpec{ID: "inbox-discovery", Kind: JobPull, DiscoverPubKey: item.PubKey, Filter: `{"kinds":[1],"#p":["` + item.PubKey + `"]}`, Every: 1}
	if err := ValidateJob(spec); err != nil {
		t.Fatal(err)
	}
	if err := service.pull(ctx, spec); err != nil {
		t.Fatal(err)
	}
}

func (p integrationPull) Query(context.Context, string, event.Filter) ([]event.Event, error) {
	return p.events, nil
}

func TestPullAndPushJobsPersistSyncStats(t *testing.T) {
	ctx := context.Background()
	store := openReplicationStore(t)
	pullEvent := signedEvent(t, 200)
	pull, err := NewService(Config{Store: store, DataDir: t.TempDir(), Pull: integrationPull{events: []event.Event{pullEvent}}, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	pullSpec := JobSpec{ID: "pull-stats", Kind: JobPull, Relays: []string{"wss://source.example"}}
	if err := pull.AddJob(ctx, pullSpec); err != nil {
		t.Fatal(err)
	}
	pullCtx, stopPull := context.WithCancel(ctx)
	pullDone := make(chan error, 1)
	go func() { pullDone <- pull.Run(pullCtx) }()
	pullStatus := waitJobStatus(t, pull, pullSpec.ID, func(status JobStatus) bool { return status.Phase == "complete" })
	if pullStatus.Stored != 1 || pullStatus.Rounds != 1 {
		t.Fatalf("pull status = %#v", pullStatus)
	}
	stopPull()
	if err := <-pullDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("pull worker exit = %v", err)
	}

	pushEvent := signedEvent(t, 201)
	if _, err := store.Save(ctx, pushEvent, storage.SaveOptions{Now: 202}); err != nil {
		t.Fatal(err)
	}
	transport := &fakePush{}
	push, err := NewService(Config{Store: store, DataDir: t.TempDir(), Push: transport, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	pushSpec := JobSpec{ID: "push-stats", Kind: JobPush, Relays: []string{"wss://destination.example"}}
	if err := push.AddJob(ctx, pushSpec); err != nil {
		t.Fatal(err)
	}
	pushCtx, stopPush := context.WithCancel(ctx)
	pushDone := make(chan error, 1)
	go func() { pushDone <- push.Run(pushCtx) }()
	pushStatus := waitJobStatus(t, push, pushSpec.ID, func(status JobStatus) bool { return status.Phase == "complete" })
	if pushStatus.Sent < 1 || pushStatus.Cursor == 0 || pushStatus.Rounds != 1 {
		t.Fatalf("push status = %#v", pushStatus)
	}
	stopPush()
	if err := <-pushDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("push worker exit = %v", err)
	}
}

func TestValidateJobRejectsInvalidFilter(t *testing.T) {
	err := ValidateJob(JobSpec{ID: "bad-filter", Kind: JobPull, Relays: []string{"wss://relay.example"}, Filter: "[]"})
	if err == nil {
		t.Fatal("invalid filter accepted")
	}
}

func waitJobStatus(t *testing.T, service *Service, id string, ready func(JobStatus) bool) JobStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		statuses, err := service.ListJobStatus(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, status := range statuses {
			if status.Spec.ID == id && ready(status) {
				return status
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %q did not reach expected state", id)
	return JobStatus{}
}

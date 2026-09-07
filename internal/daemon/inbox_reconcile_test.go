package daemon

import (
	"context"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/catalog"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestAutomaticInboxJobReconcilesPolicyChanges(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, t.TempDir()+"/tenant.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := replication.NewService(replication.Config{Store: store, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tenant := &Tenant{meta: catalog.Tenant{ID: "tenant"}, store: store, replication: service}
	owner := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	previous := policy.Policy{Owner: owner}
	current := previous
	current.Inbox.Targeted = true
	current.AllowedKinds = []int{1, 7}
	if err := tenant.reconcileAutomaticInbox(ctx, previous, current); err != nil {
		t.Fatal(err)
	}
	jobs, err := service.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].DiscoverPubKey != owner {
		t.Fatalf("jobs after enable = %#v", jobs)
	}
	if err := tenant.reconcileAutomaticInbox(ctx, previous, current); err != nil {
		t.Fatal(err)
	}
	var intents int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM work_intents WHERE event_id=? AND kind='job'`, "source-tenant").Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if intents != 1 {
		t.Fatalf("reconcile created %d job intents, want one", intents)
	}
	disabled := current
	disabled.Inbox.Targeted = false
	if err := tenant.reconcileAutomaticInbox(ctx, current, disabled); err != nil {
		t.Fatal(err)
	}
	jobs, err = service.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs after disable = %#v", jobs)
	}
	if err := tenant.reconcileAutomaticInbox(ctx, disabled, current); err != nil {
		t.Fatal(err)
	}
	jobs, err = service.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs after re-enable = %#v", jobs)
	}
}

func TestAutomaticInboxTransferFollowsCurrentOwner(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, t.TempDir()+"/tenant.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := replication.NewService(replication.Config{Store: store, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tenant := &Tenant{meta: catalog.Tenant{ID: "tenant"}, store: store, replication: service}
	oldOwner := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	newOwner := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	previous := policy.Policy{Owner: oldOwner, Inbox: policy.Inbox{Targeted: true}, AllowedKinds: []int{1}}
	if err := tenant.reconcileAutomaticInbox(ctx, policy.Policy{Owner: oldOwner}, previous); err != nil {
		t.Fatal(err)
	}
	next := previous
	next.Owner = newOwner
	if err := tenant.reconcileAutomaticInbox(ctx, previous, next); err != nil {
		t.Fatal(err)
	}
	jobs, err := service.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].DiscoverPubKey != newOwner || inboxJobOwner(jobs[0]) != newOwner {
		t.Fatalf("transfer did not update discovery owner: %#v", jobs)
	}
}

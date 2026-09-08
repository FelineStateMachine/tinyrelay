package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
)

func TestGitLiveEnabledRequiresGRASP02(t *testing.T) {
	_, tenant := testTenant(t)
	p := tenant.Policy()
	p.Features.Grasp = true
	p.Features.Grasp02 = false
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if tenant.gitLiveEnabled() {
		t.Fatal("live synchronization enabled without GRASP-02")
	}
	p.Features.Grasp02 = true
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if !tenant.gitLiveEnabled() {
		t.Fatal("live synchronization disabled with GRASP-02")
	}
}

func TestGitLivePrivateTargetRequiresConfiguredPeer(t *testing.T) {
	_, tenant := testTenant(t)
	repo := gitrelay.Repository{Private: true}
	if tenant.gitLiveAuthorized(repo, "wss://peer.example") {
		t.Fatal("unconfigured private target was authorized")
	}
	p := tenant.Policy()
	p.Features.Grasp, p.Features.Grasp02, p.Features.Grasp08 = true, true, true
	p.Reads = "members"
	p.PrivatePeers = []string{"wss://peer.example"}
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if !tenant.gitLiveAuthorized(repo, "wss://peer.example") {
		t.Fatal("configured private target was rejected")
	}
	if tenant.gitLiveAuthorized(repo, "wss://other.example") {
		t.Fatal("different private target was authorized")
	}
}

func TestGitLiveFiltersIncludeConversationRootsAndReplies(t *testing.T) {
	repo := gitrelay.Repository{Owner: strings.Repeat("a", 64), Identifier: "repo"}
	root := event.Event{Kind: event.KIND_GIT_ISSUE, ID: strings.Repeat("b", 64)}
	filters := gitLiveFiltersForRoots(repo, []event.Event{root})
	if len(filters) != 5 {
		t.Fatalf("got %d filters, want metadata, conversation and two reply filters", len(filters))
	}
	for _, key := range []string{"e", "E"} {
		found := false
		for _, filter := range filters {
			if len(filter.Tags[key]) == 1 && filter.Tags[key][0] == root.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing reply filter for %s", key)
		}
	}
}

func TestGitLiveCohortRespectsSocketCapAndRotates(t *testing.T) {
	keys := make([]string, maxGitLivePeers+7)
	for i := range keys {
		keys[i] = strings.Repeat("a", i+1)
	}
	first := gitLiveCohort(keys, 0)
	second := gitLiveCohort(keys, maxGitLivePeers)
	if len(first) != maxGitLivePeers || len(second) != maxGitLivePeers {
		t.Fatalf("cohort sizes = %d, %d; want %d", len(first), len(second), maxGitLivePeers)
	}
	_, firstHas := first[keys[maxGitLivePeers-1]]
	_, secondHas := second[keys[maxGitLivePeers-1]]
	if firstHas == secondHas {
		t.Fatal("live cohort did not rotate")
	}
}

func TestRotateGitAuthorsUsesDurableOffset(t *testing.T) {
	authors := []string{"a", "b", "c"}
	got := rotateGitAuthors(authors, 1)
	if strings.Join(got, ",") != "b,c,a" {
		t.Fatalf("rotated authors = %v", got)
	}
}

func TestStopGitLiveWorkersCancelsAndWaits(t *testing.T) {
	done := make(chan struct{})
	canceled := make(chan struct{})
	workers := map[string]gitLiveWorker{"peer": {
		cancel: func() { close(canceled); close(done) },
		done:   done,
	}}
	finished := make(chan struct{})
	go func() {
		stopGitLiveWorkers(workers)
		close(finished)
	}()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("worker was not canceled")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("stop returned before worker completion")
	}
	if len(workers) != 0 {
		t.Fatal("stopped workers remain in the registry")
	}
}

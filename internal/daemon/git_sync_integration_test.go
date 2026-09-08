package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
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

func TestLocalSyncItemsUsesBoundedInventoryWindow(t *testing.T) {
	_, tenant := testTenant(t)
	owner := tenant.Policy().Owner
	now := time.Now().Unix()
	for i := 0; i < 10001; i++ {
		item := event.Event{ID: fmt.Sprintf("%064x", i+1), PubKey: owner, CreatedAt: now - int64(i), Kind: 1, Tags: [][]string{}, Content: "sync window"}
		if _, err := tenant.store.Save(context.Background(), item, storage.SaveOptions{Now: now, SearchMode: tenant.Policy().Features.Search}); err != nil {
			t.Fatalf("save event %d: %v", i, err)
		}
	}
	items, err := tenant.localSyncItems(context.Background(), event.Filter{Authors: []string{owner}, Kinds: []int{1}})
	if !errors.Is(err, replication.ErrPullIncomplete) {
		t.Fatalf("inventory error = %v, want ErrPullIncomplete", err)
	}
	if len(items) != 10000 {
		t.Fatalf("inventory size = %d, want bounded 10000", len(items))
	}
	if strings.TrimSpace(items[0].ID) == "" {
		t.Fatal("bounded inventory returned an empty event ID")
	}
}

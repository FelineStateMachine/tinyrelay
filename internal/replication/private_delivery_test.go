package replication

import (
	"context"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestQueuedPrivateRepositoryMetadataIsNotDelivered(t *testing.T) {
	store := openReplicationStore(t)
	ctx := context.Background()
	transport := &recordingTransport{}
	handler := NewDeliveryHandler(store, transport)
	for _, kind := range []int{event.KIND_REPO, event.KIND_REPO_STATE} {
		e := signedEvent(t, 1)
		e.Kind = kind
		e.Tags = [][]string{{"d", "private"}}
		if kind == event.KIND_REPO {
			e.Tags = append(e.Tags, []string{"private", "true"})
		}
		if err := event.Sign(&e, strings.Repeat("1", 64)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Save(ctx, e, storage.SaveOptions{Now: e.CreatedAt}); err != nil {
			t.Fatal(err)
		}
		if err := handler(ctx, storage.Intent{Kind: "delivery", EventID: e.ID, Target: "wss://public.example"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(transport.events) != 0 {
		t.Fatal("private metadata escaped through a queued public delivery")
	}
}

func TestPrivateScopeLookupFailureRetainsDeliveryForRetry(t *testing.T) {
	store := openReplicationStore(t)
	ctx := context.Background()
	e := signedEvent(t, 1)
	e.Kind = event.KIND_REPO_STATE
	e.Tags = [][]string{{"d", "private"}}
	if err := event.Sign(&e, strings.Repeat("1", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, e, storage.SaveOptions{Now: e.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	announcement := e
	announcement.Kind = event.KIND_REPO
	if err := event.Sign(&announcement, strings.Repeat("1", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, announcement, storage.SaveOptions{Now: announcement.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE events SET raw='invalid JSON' WHERE id=?", announcement.ID); err != nil {
		t.Fatal(err)
	}
	transport := &recordingTransport{}
	err := NewDeliveryHandler(store, transport)(ctx, storage.Intent{Kind: "delivery", EventID: e.ID, Target: "wss://public.example"})
	if err == nil {
		t.Fatal("privacy lookup failure was acknowledged instead of retained for retry")
	}
	if len(transport.events) != 0 {
		t.Fatal("delivery proceeded after a privacy lookup failure")
	}
}

package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/catalog"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/work"
)

func TestDeliverNotificationQueuesPrivateLocalAndRecipientWorkAtomically(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "tenant.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secret, err := event.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	wrap := event.Event{CreatedAt: 10, Kind: event.KIND_WRAP, Tags: [][]string{{"p", recipient}}, Content: "cipher"}
	if err := event.Sign(&wrap, secret); err != nil {
		t.Fatal(err)
	}
	tenant := &Tenant{store: store, meta: catalog.Tenant{ID: "tenant-1"}, publicURL: "http://relay.example"}
	if err := tenant.deliverNotification(ctx, wrap, recipient); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM work_intents WHERE event_id=?`, wrap.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("queued intents = %d, want local broadcast and recipient delivery", count)
	}
	var target string
	if err := store.DB().QueryRowContext(ctx, `SELECT target FROM work_intents WHERE kind=?`, notificationDelivery).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if target != recipient {
		t.Fatalf("delivery target = %q, want recipient %q", target, recipient)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE kind=10050`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("stored a recipient's kind-10050 list locally")
	}
}

func TestNotificationRelayValidation(t *testing.T) {
	if !validRelayURL("wss://inbox.example") || !validRelayURL("ws://localhost") {
		t.Fatal("valid relay URL rejected")
	}
	if validRelayURL("https://inbox.example") || validRelayURL("relay.example") {
		t.Fatal("non-relay URL accepted")
	}
	if !sameRelay("wss://relay.example/", "wss://relay.example") {
		t.Fatal("trailing slash not normalized")
	}
}

func TestNotificationDeliveryRequiresRecipientInboxList(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "tenant.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secret, err := event.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	wrap := event.Event{CreatedAt: 10, Kind: event.KIND_WRAP, Tags: [][]string{{"p", recipient}}, Content: "cipher"}
	if err := event.Sign(&wrap, secret); err != nil {
		t.Fatal(err)
	}
	tenant := &Tenant{store: store}
	if err := tenant.deliverNotification(ctx, wrap, recipient); err != nil {
		t.Fatal(err)
	}
	handler := tenant.notificationHandlers()[notificationDelivery]
	err = handler(ctx, work.Intent{Kind: notificationDelivery, EventID: wrap.ID, Target: recipient})
	if err == nil {
		t.Fatal("delivery without kind-10050 inbox should remain retryable")
	}
}

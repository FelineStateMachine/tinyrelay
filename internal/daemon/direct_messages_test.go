package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestBrowseDirectMessagesRequiresRecipientAndPaginates(t *testing.T) {
	_, tenant := testTenant(t)
	actorSecret := strings.Repeat("a", 63) + "1"
	otherSecret := strings.Repeat("b", 63) + "2"
	actor := signedEvent(t, actorSecret, event.KIND_WRAP, 100, [][]string{{"p", strings.Repeat("c", 64)}}, "ciphertext").Tags[0][1]
	recipient := actor
	first := signedEvent(t, otherSecret, event.KIND_WRAP, 200, [][]string{{"p", recipient}}, "first")
	second := signedEvent(t, otherSecret, event.KIND_WRAP, 200, [][]string{{"p", recipient}}, "second")
	third := signedEvent(t, otherSecret, event.KIND_WRAP, 199, [][]string{{"p", recipient}}, "third")
	for _, row := range []event.Event{first, second, third} {
		if _, err := tenant.store.Save(context.Background(), row, storage.SaveOptions{Now: row.CreatedAt, SearchMode: tenant.Policy().Features.Search}); err != nil {
			t.Fatal(err)
		}
	}
	value, err := tenant.Execute(context.Background(), recipient, "browsedirectmessages", []json.RawMessage{rawJSON(map[string]any{"limit": 2})})
	if err != nil {
		t.Fatal(err)
	}
	page := value.(map[string]any)
	events := page["events"].([]event.Event)
	if len(events) != 2 || events[0].ID != first.ID || events[1].ID != second.ID {
		t.Fatalf("first page = %#v", events)
	}
	next := page["next_cursor"].(string)
	value, err = tenant.Execute(context.Background(), recipient, "browsedirectmessages", []json.RawMessage{rawJSON(map[string]any{"limit": 2, "cursor": next})})
	if err != nil {
		t.Fatal(err)
	}
	page = value.(map[string]any)
	events = page["events"].([]event.Event)
	if len(events) != 1 || events[0].ID != third.ID || page["next_cursor"].(string) != "" {
		t.Fatalf("second page = %#v", page)
	}
	if events[0].Content != "third" {
		t.Fatalf("message content changed: %q", events[0].Content)
	}
}

func TestBrowseDirectMessagesDoesNotUseOwnerBypass(t *testing.T) {
	_, tenant := testTenant(t)
	owner := tenant.Policy().Owner
	other := strings.Repeat("d", 64)
	row := signedEvent(t, strings.Repeat("e", 63)+"f", event.KIND_WRAP, 100, [][]string{{"p", other}}, "secret")
	if _, err := tenant.store.Save(context.Background(), row, storage.SaveOptions{Now: row.CreatedAt, SearchMode: tenant.Policy().Features.Search}); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.Execute(context.Background(), owner, "browsedirectmessages", []json.RawMessage{rawJSON(map[string]any{"limit": 10})}); err != nil {
		t.Fatal(err)
	}
	page := mustDirectMessagePage(t, tenant, owner)
	if len(page["events"].([]event.Event)) != 0 {
		t.Fatal("owner saw a message addressed to another user")
	}
}

func TestBrowseDirectMessagesRequiresValidActor(t *testing.T) {
	_, tenant := testTenant(t)
	for _, actor := range []string{"", "not-a-pubkey"} {
		if _, err := tenant.Execute(context.Background(), actor, "browsedirectmessages", nil); err == nil || !strings.HasPrefix(err.Error(), "auth-required:") {
			t.Fatalf("actor %q error = %v", actor, err)
		}
	}
}

func TestBrowseDirectMessagesFiltersRecipientAndKind(t *testing.T) {
	_, tenant := testTenant(t)
	actorSecret := strings.Repeat("1", 63) + "2"
	actor := signedEvent(t, actorSecret, event.KIND_WRAP, 99, nil, "").PubKey
	actorMessage := signedEvent(t, actorSecret, event.KIND_WRAP, 100, [][]string{{"p", actor}}, "recipient")
	otherRecipient := signedEvent(t, strings.Repeat("3", 63)+"4", event.KIND_WRAP, 101, [][]string{{"p", strings.Repeat("f", 64)}}, "other")
	wrongKind := signedEvent(t, strings.Repeat("5", 63)+"6", event.KIND_DM, 102, [][]string{{"p", actor}}, "legacy")
	for _, row := range []event.Event{actorMessage, otherRecipient, wrongKind} {
		if _, err := tenant.store.Save(context.Background(), row, storage.SaveOptions{Now: row.CreatedAt, SearchMode: tenant.Policy().Features.Search}); err != nil {
			t.Fatal(err)
		}
	}
	value, err := tenant.Execute(context.Background(), actor, "browsedirectmessages", nil)
	if err != nil {
		t.Fatal(err)
	}
	events := value.(map[string]any)["events"].([]event.Event)
	if len(events) != 1 || events[0].ID != actorMessage.ID {
		t.Fatalf("filtered events = %#v", events)
	}
}

func TestBrowseDirectMessagesRejectsInvalidCursor(t *testing.T) {
	_, tenant := testTenant(t)
	actor := strings.Repeat("a", 64)
	_, err := tenant.Execute(context.Background(), actor, "browsedirectmessages", []json.RawMessage{rawJSON(map[string]any{"cursor": "broken"})})
	if err == nil || !strings.HasPrefix(err.Error(), "invalid: direct message cursor") {
		t.Fatalf("cursor error = %v", err)
	}
}

func mustDirectMessagePage(t *testing.T, tenant *Tenant, actor string) map[string]any {
	t.Helper()
	value, err := tenant.Execute(context.Background(), actor, "browsedirectmessages", nil)
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)
}

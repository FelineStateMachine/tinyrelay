package daemon

import (
	"strconv"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
)

func TestBrowseThreadReturnsNewestAuthorEditsOutsideCursor(t *testing.T) {
	h := newRoomHarness(t)
	h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "general"}}, "")
	root := h.must("alice", event.KIND_THREAD, [][]string{{"h", "general"}}, "original root")
	reply := h.must("bob", event.KIND_THREAD_REPLY, [][]string{{"h", "general"}, {"e", root.ID}}, "original reply")

	cursor := collaborationCursor(collaborationItemFrom(reply))

	publishAt := func(who string, createdAt int64, target event.Event, content string) event.Event {
		e := h.sign(who, event.KIND_CONTENT_EDIT, [][]string{{"h", "general"}, {"e", target.ID}}, content)
		e.CreatedAt = createdAt
		if err := event.Sign(&e, h.secrets[who]); err != nil {
			t.Fatal(err)
		}
		if _, err := h.tenant.router.Publish(h.ctx, e, relay.Session{PubKeys: []string{h.keys[who]}, RelayURL: h.tenant.RelayURL()}); err != nil {
			t.Fatal(err)
		}
		return e
	}
	old := publishAt("alice", time.Now().Unix()+1, root, "root v1")
	newest := publishAt("alice", old.CreatedAt+1, root, "root v2")
	replyEdit := publishAt("bob", newest.CreatedAt+1, reply, "reply v1")

	page, err := h.browse("bob", "browsethread", map[string]any{"id": "general", "event": root.ID, "cursor": cursor, "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	edits, ok := page["edits"].([]any)
	if !ok {
		t.Fatalf("edits = %#v", page["edits"])
	}
	found := map[string]bool{}
	for _, raw := range edits {
		row := raw.(map[string]any)
		found[row["id"].(string)] = true
	}
	if !found[newest.ID] || found[replyEdit.ID] || found[old.ID] {
		t.Fatalf("edits = %#v, want newest visible root only", edits)
	}

	forged := h.sign("alice", event.KIND_CONTENT_EDIT, [][]string{{"h", "general"}, {"e", reply.ID}}, "wrong author")
	// The target belongs to Bob, so the author filter must exclude this edit.
	if _, err := h.tenant.router.Publish(h.ctx, forged, relay.Session{PubKeys: []string{h.keys["alice"]}, RelayURL: h.tenant.RelayURL()}); err != nil {
		t.Fatal(err)
	}
	page, err = h.browse("bob", "browsethread", map[string]any{"id": "general", "event": root.ID})
	if err != nil {
		t.Fatal(err)
	}
	found = map[string]bool{}
	for _, raw := range page["edits"].([]any) {
		found[raw.(map[string]any)["id"].(string)] = true
		if raw.(map[string]any)["content"] == "wrong author" {
			t.Fatal("forged edit was returned")
		}
	}
	if !found[replyEdit.ID] || !found[newest.ID] {
		t.Fatalf("unpaged edits = %#v, want root and reply", page["edits"])
	}
}

func TestRoomEditsKeepIndependentTargetsAndUseLastReference(t *testing.T) {
	h := newRoomHarness(t)
	h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "general"}}, "")
	first := h.must("alice", event.KIND_CHAT, [][]string{{"h", "general"}}, "first")
	second := h.must("alice", event.KIND_CHAT, [][]string{{"h", "general"}}, "second")
	wanted := h.must("alice", event.KIND_CONTENT_EDIT, [][]string{{"h", "general"}, {"e", first.ID}}, "first replacement")
	var latest event.Event
	for i := 0; i < 105; i++ {
		// The first reference is context; only the last reference is edited.
		latest = signedEvent(t, h.secrets["alice"], event.KIND_CONTENT_EDIT, first.CreatedAt+int64(i)+1, [][]string{{"h", "general"}, {"e", first.ID}, {"e", second.ID}}, "second replacement "+strconv.Itoa(i))
		if err := publishAs(t, h.tenant, latest); err != nil {
			t.Fatal(err)
		}
	}
	edits, err := h.tenant.roomEdits(h.ctx, h.keys["alice"], "general", []event.Event{first, second})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, edit := range edits {
		found[edit.ID] = true
	}
	if len(edits) != 2 || !found[wanted.ID] || !found[latest.ID] {
		t.Fatalf("lost independent target edit: %+v", edits)
	}
}

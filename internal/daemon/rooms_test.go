package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
)

type roomHarness struct {
	t       *testing.T
	app     *App
	tenant  *Tenant
	ctx     context.Context
	secrets map[string]string
	keys    map[string]string
}

func newRoomHarness(t *testing.T) *roomHarness {
	t.Helper()
	app, tenant := testTenant(t)
	h := &roomHarness{t: t, app: app, tenant: tenant, ctx: context.Background(), secrets: map[string]string{}, keys: map[string]string{}}
	for i, name := range []string{"owner", "alice", "bob", "stranger"} {
		h.secrets[name] = strings.Repeat("0", 63) + string(rune('1'+i))
		h.keys[name], _ = event.PublicKey(h.secrets[name])
	}
	for _, name := range []string{"alice", "bob"} {
		if _, err := tenant.Execute(h.ctx, h.keys["owner"], "setmember", []json.RawMessage{rawJSON(h.keys[name]), rawJSON(map[string]any{"role": "member"})}); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func rawJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (h *roomHarness) sign(who string, kind int, tags [][]string, content string) event.Event {
	h.t.Helper()
	e := event.Event{Kind: kind, CreatedAt: time.Now().Unix(), Tags: tags, Content: content}
	if err := event.Sign(&e, h.secrets[who]); err != nil {
		h.t.Fatal(err)
	}
	return e
}

func (h *roomHarness) publish(who string, kind int, tags [][]string, content string) (event.Event, error) {
	h.t.Helper()
	e := h.sign(who, kind, tags, content)
	_, err := h.tenant.router.Publish(h.ctx, e, relay.Session{PubKeys: []string{e.PubKey}, RelayURL: h.tenant.RelayURL()})
	return e, err
}

func (h *roomHarness) must(who string, kind int, tags [][]string, content string) event.Event {
	h.t.Helper()
	e, err := h.publish(who, kind, tags, content)
	if err != nil {
		h.t.Fatalf("publish kind %d from %s: %v", kind, who, err)
	}
	return e
}

// projectRooms waits for the tenant worker to finish the queued room
// projections, so records and notices are observed the way clients see them.
func (h *roomHarness) projectRooms() {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var pending int
		if err := h.tenant.store.DB().QueryRowContext(h.ctx, `SELECT COUNT(*) FROM work_intents WHERE kind='records-projection' AND target='room' AND state IN ('pending','running')`).Scan(&pending); err != nil {
			h.t.Fatal(err)
		}
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%d room projections still queued", pending)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (h *roomHarness) query(who string, f event.Filter) []event.Event {
	h.t.Helper()
	s := relay.Session{RelayURL: h.tenant.RelayURL()}
	if who != "" {
		s.PubKeys = []string{h.keys[who]}
	}
	rows, err := h.tenant.Query(h.ctx, []event.Filter{f}, s)
	if err != nil {
		h.t.Fatal(err)
	}
	return rows
}

func (h *roomHarness) browse(who, method string, params map[string]any) (map[string]any, error) {
	h.t.Helper()
	actor := ""
	if who != "" {
		actor = h.keys[who]
	}
	result, err := h.tenant.Execute(h.ctx, actor, method, []json.RawMessage{rawJSON(params)})
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(result)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

func TestRoomsLifecycleRecordsAndBrowse(t *testing.T) {
	h := newRoomHarness(t)
	if _, err := h.publish("stranger", event.KIND_CREATE_GROUP, [][]string{{"h", "general"}}, ""); err == nil {
		t.Fatal("stranger created a room")
	}
	h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "general"}, {"name", "General"}}, "")
	h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "secret"}, {"name", "Secret"}, {"closed"}}, "")
	// A tenant member joins an open room on their first message; a members-only room refuses them.
	hello := h.must("bob", event.KIND_CHAT, [][]string{{"h", "general"}}, "hello")
	if role, _ := h.tenant.community.RoomRole(h.ctx, "general", h.keys["bob"]); role != community.RoomRoleMember {
		t.Fatalf("bob role = %q", role)
	}
	if _, err := h.publish("bob", event.KIND_CHAT, [][]string{{"h", "secret"}}, "psst"); err == nil {
		t.Fatal("outsider wrote into a members-only room")
	}
	if _, err := h.publish("bob", event.KIND_CHAT, [][]string{{"h", "missing"}}, "?"); err == nil {
		t.Fatal("unknown room accepted")
	}
	if _, err := h.publish("alice", event.KIND_ROOM_MEMBER_ADDED, [][]string{{"h", "general"}, {"p", h.keys["bob"]}}, ""); err == nil {
		t.Fatal("client published a member notice")
	}
	// Presence is accepted from members and never stored.
	h.must("bob", event.KIND_ROOM_PRESENCE, [][]string{{"h", "general"}}, "")
	if rows := h.query("bob", event.Filter{Kinds: []int{event.KIND_ROOM_PRESENCE}}); len(rows) != 0 {
		t.Fatalf("presence stored: %d", len(rows))
	}
	// Room-scoped metadata edits use room roles, not tenant moderation.
	h.must("alice", event.KIND_EDIT_METADATA, [][]string{{"h", "general"}, {"about", "Everyone"}}, "")
	if room, _ := h.tenant.community.Room(h.ctx, "general"); room.About != "Everyone" {
		t.Fatalf("room edit lost: %+v", room)
	}
	if _, err := h.publish("bob", event.KIND_EDIT_METADATA, [][]string{{"h", "general"}, {"name", "Nope"}}, ""); err == nil {
		t.Fatal("member edited the room")
	}
	// Relay records and member notices follow the durable projection.
	h.projectRooms()
	meta := h.query("", event.Filter{Kinds: []int{event.KIND_GROUP_METADATA}, Tags: map[string][]string{"d": {"general"}}})
	if len(meta) != 1 || meta[0].PubKey != h.tenant.records.PublicKey() || event.Tag(meta[0], "name") != "General" || event.Tag(meta[0], "about") != "Everyone" {
		t.Fatalf("39000 = %+v", meta)
	}
	admins := h.query("", event.Filter{Kinds: []int{event.KIND_GROUP_ADMINS}, Tags: map[string][]string{"d": {"general"}}})
	if len(admins) != 1 || len(admins[0].Tags) < 3 || admins[0].Tags[2][1] != h.keys["alice"] || admins[0].Tags[2][2] != "owner" {
		t.Fatalf("39001 = %+v", admins)
	}
	members := h.query("", event.Filter{Kinds: []int{event.KIND_GROUP_MEMBERS}, Tags: map[string][]string{"d": {"general"}}})
	if len(members) != 1 || len(event.TagValues(members[0], "p")) != 2 {
		t.Fatalf("39002 = %+v", members)
	}
	notices := h.query("", event.Filter{Kinds: []int{event.KIND_ROOM_MEMBER_ADDED}, Tags: map[string][]string{"h": {"general"}}})
	if len(notices) != 2 || notices[0].PubKey != h.tenant.records.PublicKey() {
		t.Fatalf("44100 notices = %+v", notices)
	}
	if hidden := h.query("bob", event.Filter{Kinds: []int{event.KIND_GROUP_METADATA}, Tags: map[string][]string{"d": {"secret"}}}); len(hidden) != 0 {
		t.Fatal("members-only room record served to an outsider")
	}
	if shown := h.query("owner", event.Filter{Kinds: []int{event.KIND_GROUP_METADATA}, Tags: map[string][]string{"d": {"secret"}}}); len(shown) != 1 {
		t.Fatal("members-only room record hidden from the tenant owner")
	}
	// Browse calls apply the same visibility.
	list, err := h.browse("bob", "browserooms", nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]map[string]any{}
	for _, item := range list["items"].([]any) {
		row := item.(map[string]any)
		ids[row["id"].(string)] = row
	}
	if _, ok := ids["secret"]; ok || ids["general"] == nil || ids["main"] == nil {
		t.Fatalf("rooms for bob = %v", ids)
	}
	if ids["general"]["members"].(float64) != 2 || ids["general"]["role"] != "member" || ids["general"]["last_message_at"].(float64) < float64(hello.CreatedAt) {
		t.Fatalf("general summary = %v", ids["general"])
	}
	ownerList, _ := h.browse("owner", "browserooms", nil)
	if len(ownerList["items"].([]any)) != 3 {
		t.Fatalf("owner sees %d rooms", len(ownerList["items"].([]any)))
	}
	if _, err := h.browse("bob", "browseroom", map[string]any{"id": "secret"}); err == nil || !strings.HasPrefix(err.Error(), "restricted:") {
		t.Fatalf("members-only room browsed by an outsider: %v", err)
	}
	if _, err := h.browse("", "browseroom", map[string]any{"id": "secret"}); err == nil || !strings.HasPrefix(err.Error(), "auth-required:") {
		t.Fatalf("members-only room browsed anonymously: %v", err)
	}
	room, err := h.browse("bob", "browseroom", map[string]any{"id": "general", "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(room["members"].([]any)) != 2 || room["room"].(map[string]any)["name"] != "General" {
		t.Fatalf("room detail = %v", room)
	}
	messages := room["messages"].([]any)
	if len(messages) != 2 || room["next_cursor"] == "" {
		t.Fatalf("messages = %d cursor=%v", len(messages), room["next_cursor"])
	}
	more, err := h.browse("bob", "browseroom", map[string]any{"id": "general", "limit": 2, "cursor": room["next_cursor"]})
	if err != nil || len(more["messages"].([]any)) == 0 {
		t.Fatalf("second page = %v %v", more, err)
	}
	// Threads: a kind 11 root and its kind 12 replies.
	root := h.must("alice", event.KIND_THREAD, [][]string{{"h", "general"}, {"subject", "Plans"}}, "What next?")
	reply := h.must("bob", event.KIND_THREAD_REPLY, [][]string{{"h", "general"}, {"e", root.ID}, {"p", h.keys["alice"]}}, "Ship it")
	thread, err := h.browse("bob", "browsethread", map[string]any{"id": "general", "event": root.ID})
	if err != nil {
		t.Fatal(err)
	}
	if thread["root"].(map[string]any)["id"] != root.ID || len(thread["replies"].([]any)) != 1 || thread["replies"].([]any)[0].(map[string]any)["id"] != reply.ID {
		t.Fatalf("thread = %v", thread)
	}
	if _, err := h.browse("bob", "browsethread", map[string]any{"id": "general", "event": hello.ID}); err != nil {
		t.Fatalf("chat roots are threadable: %v", err)
	}
	// Device notices name the room and link to it.
	mention := h.sign("bob", event.KIND_CHAT, [][]string{{"h", "general"}, {"p", h.keys["alice"]}}, "ping alice")
	pushes := h.tenant.pushNotices(h.ctx, mention)
	if len(pushes) != 1 || pushes[0].recipient != h.keys["alice"] || pushes[0].category != pushMentions || pushes[0].body != "Mentioned you in General: ping alice" || !strings.HasSuffix(pushes[0].url, "/rooms/general") {
		t.Fatalf("push notices = %+v", pushes)
	}
	if pushes := h.tenant.pushNotices(h.ctx, reply); len(pushes) != 1 || pushes[0].category != pushReplies || !strings.HasPrefix(pushes[0].body, "Replied to you in General:") {
		t.Fatalf("reply notices = %+v", pushes)
	}
	// Deleting the room retires its records and history.
	if _, err := h.publish("bob", event.KIND_DELETE_GROUP, [][]string{{"h", "general"}}, ""); err == nil {
		t.Fatal("member deleted the room")
	}
	h.must("alice", event.KIND_DELETE_GROUP, [][]string{{"h", "general"}}, "")
	h.projectRooms()
	if rows := h.query("owner", event.Filter{Kinds: []int{event.KIND_GROUP_METADATA, event.KIND_CHAT}, Tags: map[string][]string{"d": {"general"}}}); len(rows) != 0 {
		t.Fatalf("deleted room records remain: %d", len(rows))
	}
	if _, err := h.browse("alice", "browseroom", map[string]any{"id": "general"}); err == nil {
		t.Fatal("deleted room still browsable")
	}
}

func TestTypedRoomReaderPreservesSignedEvents(t *testing.T) {
	h := newRoomHarness(t)
	message := h.must("alice", event.KIND_CHAT, [][]string{{"h", "main"}, {"imeta", "url https://files.example/report.pdf", "m application/pdf", "filename report.pdf"}}, "typed room read")
	edit := h.must("alice", event.KIND_CONTENT_EDIT, [][]string{{"h", "main"}, {"e", message.ID}}, "typed room edit")
	h.projectRooms()

	typed, err := h.tenant.ReadRoom(h.ctx, h.keys["alice"], "main", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	legacyValue, err := h.browse(h.keys["alice"], "browseroom", map[string]any{"id": "main", "limit": 100})
	if err != nil {
		t.Fatal(err)
	}
	var typedMessage *event.Event
	for i := range typed.Messages {
		if typed.Messages[i].ID == message.ID {
			typedMessage = &typed.Messages[i]
			break
		}
	}
	if typedMessage == nil || typedMessage.Sig != message.Sig || len(typedMessage.Tags) != 2 {
		t.Fatalf("typed messages lost signed event: %+v", typed.Messages)
	}
	if len(typed.Edits) != 1 || typed.Edits[0].ID != edit.ID || typed.Edits[0].Sig != edit.Sig {
		t.Fatalf("typed edits lost latest signed edit: %+v", typed.Edits)
	}
	legacyMessages := legacyValue["messages"].([]any)
	if len(legacyMessages) == 0 {
		t.Fatal("legacy messages empty")
	}
	var legacyMessage map[string]any
	for _, value := range legacyMessages {
		candidate, _ := value.(map[string]any)
		if candidate["id"] == message.ID {
			legacyMessage = candidate
			break
		}
	}
	if legacyMessage == nil || legacyMessage["sig"] != message.Sig {
		t.Fatalf("legacy messages lost signed event: %v", legacyMessages)
	}
	legacyEdits := legacyValue["edits"].([]any)
	if len(legacyEdits) != 1 || legacyEdits[0].(map[string]any)["sig"] != edit.Sig {
		t.Fatalf("legacy edits lost latest signed edit: %v", legacyEdits)
	}
}

func TestRoomsPolicyCapAndSlugMirror(t *testing.T) {
	h := newRoomHarness(t)
	if h.tenant.Policy().Rooms != 64 {
		t.Fatalf("default room allowance = %d", h.tenant.Policy().Rooms)
	}
	if _, err := h.tenant.Execute(h.ctx, h.keys["owner"], "setpolicy", []json.RawMessage{rawJSON(map[string]any{"rooms": 1})}); err != nil {
		t.Fatal(err)
	}
	h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "one"}}, "")
	if _, err := h.publish("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "two"}}, ""); err == nil || !strings.Contains(err.Error(), "allows 1 rooms") {
		t.Fatalf("cap not enforced: %v", err)
	}
	if _, err := h.tenant.Execute(h.ctx, h.keys["owner"], "setpolicy", []json.RawMessage{rawJSON(map[string]any{"rooms": -1})}); err == nil {
		t.Fatal("negative room allowance accepted")
	}
	slug, err := h.tenant.community.Room(h.ctx, "main")
	if err != nil || !slug.Live() || slug.Access != community.RoomOpen {
		t.Fatalf("slug room = %+v %v", slug, err)
	}
	if role, _ := h.tenant.community.RoomRole(h.ctx, "main", h.keys["alice"]); role != community.RoomRoleMember {
		t.Fatalf("alice slug role = %q", role)
	}
	p := h.tenant.Policy()
	p.Reads = "members"
	if err := h.tenant.applyPolicy(h.ctx, p); err != nil {
		t.Fatal(err)
	}
	if slug, _ := h.tenant.community.Room(h.ctx, "main"); slug.Access != community.RoomMembers {
		t.Fatalf("slug access after policy change = %q", slug.Access)
	}
	if _, err := h.tenant.Execute(h.ctx, h.keys["owner"], "removemember", []json.RawMessage{rawJSON(h.keys["alice"])}); err != nil {
		t.Fatal(err)
	}
	if role, _ := h.tenant.community.RoomRole(h.ctx, "main", h.keys["alice"]); role != "" {
		t.Fatalf("removed member keeps slug role %q", role)
	}
	if role, _ := h.tenant.community.RoomRole(h.ctx, "one", h.keys["alice"]); role != "" {
		t.Fatalf("removed member keeps room role %q", role)
	}
}

func TestRoomStreamDeliversLiveMessages(t *testing.T) {
	h := newRoomHarness(t)
	h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "general"}, {"name", "General"}}, "")
	h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "secret"}, {"closed"}}, "")
	server := httptest.NewServer(h.app)
	defer server.Close()

	anonymous, err := http.Get(server.URL + "/rooms/general/stream")
	if err != nil {
		t.Fatal(err)
	}
	anonymous.Body.Close()
	if anonymous.StatusCode != http.StatusOK {
		t.Fatalf("open room stream without a session: %d", anonymous.StatusCode)
	}
	closed, err := http.Get(server.URL + "/rooms/secret/stream")
	if err != nil {
		t.Fatal(err)
	}
	closed.Body.Close()
	if closed.StatusCode != http.StatusUnauthorized {
		t.Fatalf("members-only room stream without a session: %d", closed.StatusCode)
	}
	missing, err := http.Get(server.URL + "/rooms/missing/stream")
	if err != nil {
		t.Fatal(err)
	}
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown room stream: %d", missing.StatusCode)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/rooms/general/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	signRequest(t, req, "")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(res.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != ": room general\n" {
		t.Fatalf("opening comment %q %v", line, err)
	}
	sent := h.must("alice", event.KIND_CHAT, [][]string{{"h", "general"}}, "live")
	h.must("alice", event.KIND_CHAT, [][]string{{"h", "secret"}}, "elsewhere")
	deadline := time.After(5 * time.Second)
	lines := make(chan string, 8)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				close(lines)
				return
			}
			lines <- line
		}
	}()
	var got event.Event
	for got.ID == "" {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream ended early")
			}
			if line == "event: message\n" {
				data := <-lines
				if err := json.Unmarshal([]byte(strings.TrimPrefix(data, "data: ")), &got); err != nil {
					t.Fatalf("frame %q: %v", data, err)
				}
			}
		case <-deadline:
			t.Fatal("no message within 5s")
		}
	}
	if got.ID != sent.ID || got.Content != "live" {
		t.Fatalf("streamed %+v, want %s", got, sent.ID)
	}
	cancel()
	select {
	case _, ok := <-lines:
		if ok {
			// Drain until the server closes the body.
			for range lines {
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not close after cancel")
	}
}

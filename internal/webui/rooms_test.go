package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

var (
	roomOwner  = strings.Repeat("a", 64)
	roomAgent  = strings.Repeat("b", 64)
	roomGuest  = strings.Repeat("c", 64)
	roomHello  = strings.Repeat("1", 64)
	roomChat   = strings.Repeat("2", 64)
	roomThread = strings.Repeat("3", 64)
	roomReply  = strings.Repeat("4", 64)
	roomLike   = strings.Repeat("5", 64)
)

// roomsBackend answers the room browse calls for an open room and a
// members-only room. Guests see the open room only.
type roomsBackend struct {
	fakeBackend
	calls []string
}

func roomEvent(id, pubkey string, kind int, createdAt int64, content string, tags ...[]string) map[string]any {
	return map[string]any{"id": id, "pubkey": pubkey, "kind": kind, "created_at": createdAt, "content": content, "tags": tags, "sig": ""}
}

func roomRecord(id, name, access, role string) map[string]any {
	return map[string]any{"id": id, "name": name, "about": "Where " + name + " happens.", "picture": "", "access": access, "created_by": roomOwner, "created_at": 1757200000, "event_id": "", "members": 2, "last_message_at": 1757203600, "role": role}
}

func (b *roomsBackend) Query(_ context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	b.calls = append(b.calls, method+":"+actor)
	q := map[string]any{}
	if len(params) > 0 {
		_ = json.Unmarshal(params[0], &q)
	}
	role := ""
	if actor != "" {
		role = "owner"
	}
	id, _ := q["id"].(string)
	switch method {
	case "browserooms":
		items := []any{roomRecord("general", "General", "open", role)}
		if actor != "" {
			items = append(items, roomRecord("build", "Build", "members", role))
		}
		return map[string]any{"items": items, "next_cursor": ""}, nil
	case "browseroom", "browsethread":
		if id == "build" && actor == "" {
			return nil, errors.New("auth-required: this room is members-only")
		}
		if id != "general" && id != "build" {
			return nil, errors.New("not found: room")
		}
	default:
		return nil, nil
	}
	members := []any{map[string]any{"pubkey": roomOwner, "role": "owner", "added_at": 1757200000}, map[string]any{"pubkey": roomAgent, "role": "member", "added_at": 1757200100, "agent": true}}
	root := roomEvent(roomThread, roomAgent, 11, 1757203000, "Release notes draft", []string{"h", id})
	reply := roomEvent(roomReply, roomOwner, 12, 1757203300, "Looks good", []string{"h", id}, []string{"e", roomThread}, []string{"p", roomAgent})
	if method == "browsethread" {
		return map[string]any{"room": roomRecord(id, "General", "open", ""), "root": root, "replies": []any{reply}, "next_cursor": ""}, nil
	}
	messages := []any{
		roomEvent(strings.Repeat("a", 64), roomAgent, 40003, 1757203650, "hello again, see https://example.com/docs. and nostr:npub1ttrypewl3au52wqux86r22yt506c077k3maj02a0jste97wrvd5sjfutc2", []string{"h", id}, []string{"e", roomHello}),
		roomEvent(strings.Repeat("b", 64), roomOwner, 40003, 1757203660, "not my message", []string{"h", id}, []string{"e", roomHello}),
		roomEvent(roomLike, roomOwner, 7, 1757203600, "+", []string{"h", id}, []string{"e", roomChat}),
		reply,
		root,
		roomEvent(roomChat, roomOwner, 9, 1757202000, "@agent take a look <script>alert(1)</script>", []string{"h", id}, []string{"p", roomAgent}),
		roomEvent(roomHello, roomAgent, 9, 1757201000, "hello, see https://example.com/docs. and nostr:npub1ttrypewl3au52wqux86r22yt506c077k3maj02a0jste97wrvd5sjfutc2", []string{"h", id}),
	}
	return map[string]any{"room": roomRecord(id, strings.ToUpper(id[:1])+id[1:], map[string]string{"general": "open", "build": "members"}[id], role), "members": members, "messages": messages, "next_cursor": "cursor-1"}, nil
}

func roomsApp(t *testing.T, actor string) (*App, *roomsBackend) {
	t.Helper()
	backend := &roomsBackend{fakeBackend: fakeBackend{policy: policy.Defaults(roomOwner)}}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return actor, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return app, backend
}

func roomsPage(t *testing.T, app *App, path string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s: status=%d", path, recorder.Code)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "class=") {
		t.Errorf("%s carries a class attribute", path)
	}
	content := body[strings.Index(body, `id="content"`):strings.Index(body, `id="aside"`)]
	if sections := strings.Count(content, "<section"); sections != 1 {
		t.Errorf("%s renders %d sections in the content column, want 1", path, sections)
	}
	return body
}

func TestRoomsListRendersRoomsRailAndCreateForm(t *testing.T) {
	app, backend := roomsApp(t, roomOwner)
	body := roomsPage(t, app, "/rooms")
	for _, want := range []string{`<table id="rows">`, `<a href="/rooms/general">General</a>`, `<a href="/rooms/build">Build</a>`, `members only <small>Where Build happens.</small>`, `<room-create>`, `<a href="/rooms"><b>rooms</b></a>`, `<nav id="room-list"><a href="/rooms/general">General`, `<a href="/rooms#new-room">+ new room</a>`, `<p id="rail-tools"><a href="/">&larr; relay</a>`, `<h4>Rooms</h4>`, `<td>2</td>`} {
		if !strings.Contains(body, want) {
			t.Errorf("rooms list missing %q", want)
		}
	}
	if backend.calls[0] != "browserooms:"+roomOwner {
		t.Fatalf("calls = %v", backend.calls)
	}
	guestApp, _ := roomsApp(t, "")
	body = roomsPage(t, guestApp, "/rooms")
	if strings.Contains(body, "<room-create>") || !strings.Contains(body, `<a href="/signin">Sign in</a> to create a room.`) || strings.Contains(body, `href="/rooms/build"`) {
		t.Fatalf("guest rooms list: %s", body[strings.Index(body, `id="content"`):])
	}
	// The relay rail lists rooms first in the conversation group.
	home := roomsPage(t, app, "/")
	rooms, inbox, search := strings.Index(home, `href="/rooms"`), strings.Index(home, `href="/inbox"`), strings.Index(home, `href="/search"`)
	if rooms < 0 || inbox < 0 || !(search < rooms && rooms < inbox) {
		t.Fatalf("relay rail order: search=%d rooms=%d inbox=%d", search, rooms, inbox)
	}
}

func TestRoomPageRendersMessagesOldestFirstWithMarkers(t *testing.T) {
	app, backend := roomsApp(t, roomOwner)
	body := roomsPage(t, app, "/rooms/general")
	for _, want := range []string{
		`<h1>General <small>open room</small></h1>`, `<p>Where General happens.</p>`,
		`<a href="/rooms/general?cursor=cursor-1">load earlier</a>`,
		`<room-message id="msg-` + roomHello + `" data-id="` + roomHello + `" data-kind="9" data-pubkey="` + roomAgent + `" data-agent data-edited>`,
		`<a href="https://example.com/docs" rel="noopener">https://example.com/docs</a>.`,
		`<a href="/open?target=nostr%3Anpub1ttrypewl3au52wqux86r22yt506c077k3maj02a0jste97wrvd5sjfutc2">nostr:npub1`,
		`&lt;script&gt;alert(1)&lt;/script&gt;`,
		`<span>to <nostr-name pubkey="` + roomAgent + `"`,
		`<span data-reaction="&#43;1">&#43;1 1</span>`,
		`<div><p>hello again, see <a href="https://example.com/docs" rel="noopener">`, `<span data-edited>edited</span>`,
		`<a href="/rooms/general/thread/` + roomThread + `">thread | 1 reply</a>`,
		`<a href="/rooms/general/thread/` + roomThread + `">in thread</a>`,
		`<room-live room="general"></room-live>`,
		`<room-compose room="general" pubkey="` + roomOwner + `">`,
		`<noscript><p>Sending needs JavaScript and a connected signer.</p></noscript>`,
		`<ul id="members"><li><nostr-name pubkey="` + roomOwner + `"`, `<li data-agent><nostr-name pubkey="` + roomAgent + `"`, `<small>member | agent</small>`,
		`<room-action room="general" kind="9000">`, `<room-action room="general" kind="9002">`, `<room-action room="general" kind="9022">`,
		`<nav id="room-list"><a href="/rooms/general" aria-current="page">General`,
		`&raquo; <page-link url="http://relay.example/rooms/general" title="Copy the page address">rooms/general</page-link></span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("room page missing %q", want)
		}
	}
	hello, chat, thread, reply := strings.Index(body, `id="msg-`+roomHello), strings.Index(body, `id="msg-`+roomChat), strings.Index(body, `id="msg-`+roomThread), strings.Index(body, `id="msg-`+roomReply)
	if !(hello < chat && chat < thread && thread < reply) {
		t.Fatalf("messages are not oldest first: %d %d %d %d", hello, chat, thread, reply)
	}
	if strings.Contains(body, `data-kind="7"`) || strings.Contains(body, "<script>alert") {
		t.Fatal("reactions rendered as messages or content unescaped")
	}
	if len(backend.calls) != 2 || backend.calls[0] != "browseroom:"+roomOwner || backend.calls[1] != "browserooms:"+roomOwner {
		t.Fatalf("calls = %v", backend.calls)
	}
	guestApp, _ := roomsApp(t, "")
	body = roomsPage(t, guestApp, "/rooms/general")
	if strings.Contains(body, "<room-compose") || strings.Contains(body, "<room-action") || !strings.Contains(body, `<a href="/signin">Sign in</a> to send messages.`) || !strings.Contains(body, `id="msg-`+roomHello) {
		t.Fatalf("guest room page: %s", body[strings.Index(body, `id="content"`):])
	}
}

func TestMembersOnlyRoomIsHiddenFromGuests(t *testing.T) {
	guestApp, _ := roomsApp(t, "")
	body := roomsPage(t, guestApp, "/rooms/build")
	if !strings.Contains(body, `<p role="alert">auth-required: this room is members-only</p>`) || strings.Contains(body, "<room-message") || strings.Contains(body, "<room-compose") || !strings.Contains(body, `<a href="/signin">Sign in</a> to read this room.`) {
		t.Fatalf("guest saw a members-only room: %s", body[strings.Index(body, `id="content"`):])
	}
	if !strings.Contains(body, `<h1>#build</h1>`) || strings.Contains(body, "Where Build happens") {
		t.Fatal("guest error page leaked room details")
	}
	app, _ := roomsApp(t, roomOwner)
	body = roomsPage(t, app, "/rooms/build")
	if !strings.Contains(body, `<h1>Build <small>members only</small></h1>`) || !strings.Contains(body, `id="msg-`+roomHello) {
		t.Fatal("owner did not see the members-only room")
	}
}

func TestThreadPageRendersRootRepliesAndReplyCompose(t *testing.T) {
	app, _ := roomsApp(t, roomOwner)
	body := roomsPage(t, app, "/rooms/general/thread/"+roomThread)
	for _, want := range []string{
		`<p id="crumbs"><a href="/rooms/general">General</a></p>`, `<h1>Thread <small>1 reply</small></h1>`,
		`<div id="root"><room-message id="msg-` + roomThread + `"`, `<div id="messages">`, `id="msg-` + roomReply + `"`,
		`<room-live room="general" root="` + roomThread + `"></room-live>`,
		`<room-compose room="general" pubkey="` + roomOwner + `" kind="12" root="` + roomThread + `" root-pubkey="` + roomAgent + `">`,
		`&raquo; <page-link url="http://relay.example/rooms/general/thread/` + roomThread + `" title="Copy the page address">rooms/general/thread/` + shortID(roomThread) + `</page-link></span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("thread page missing %q", want)
		}
	}
	if strings.Contains(body, `">thread</a>`) || strings.Contains(body, ">in thread</a>") {
		t.Fatal("thread page links back to itself")
	}
}

func TestRoomRoutesRejectMalformedPaths(t *testing.T) {
	for path, want := range map[string]roomPath{
		"/rooms":                        {tab: "rooms"},
		"/rooms/general":                {tab: "room", id: "general"},
		"/rooms/General":                {},
		"/rooms/a/b":                    {},
		"/rooms/a/thread/xyz":           {},
		"/rooms/a/thread/" + roomThread: {tab: "thread", id: "a", event: roomThread},
		"/rooms/a/stream":               {},
	} {
		if got := roomRoute(path); got != want {
			t.Errorf("roomRoute(%q) = %+v, want %+v", path, got, want)
		}
	}
	app, backend := roomsApp(t, roomOwner)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/rooms/Not%20A%20Room", nil))
	for _, call := range backend.calls {
		if strings.HasPrefix(call, "browseroom") {
			t.Fatalf("malformed room path reached the backend: %v", backend.calls)
		}
	}
}

func TestRoomContentLinksAndEscapes(t *testing.T) {
	got := string(roomContent(`<b>hi</b> https://a.example/x?y=1) then nostr:note1abc, done`))
	want := `&lt;b&gt;hi&lt;/b&gt; <a href="https://a.example/x?y=1" rel="noopener">https://a.example/x?y=1</a>) then <a href="/open?target=nostr%3Anote1abc">nostr:note1abc</a>, done`
	if got != want {
		t.Fatalf("roomContent:\n got %s\nwant %s", got, want)
	}
}

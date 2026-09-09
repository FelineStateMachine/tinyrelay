package community

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type roomFixture struct {
	ctx   context.Context
	store *storage.Store
	svc   *Service
	rooms int
	keys  map[string]string
}

func newRoomFixture(t *testing.T) *roomFixture {
	t.Helper()
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &roomFixture{ctx: ctx, store: st, rooms: 64, keys: map[string]string{}}
	for i, name := range []string{"owner", "alice", "bob", "carol", "dave", "stranger"} {
		f.keys[name] = strings.Repeat("0", 63) + string(rune('1'+i))
	}
	f.svc, err = New(ctx, st, f.pub("owner"))
	if err != nil {
		t.Fatal(err)
	}
	f.svc.ConfigurePolicy(func() policy.Policy {
		p := policy.Defaults(f.pub("owner"))
		p.Rooms = f.rooms
		return p
	})
	if err := f.svc.EnsureSlugRoom(ctx, "main", RoomOpen); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "bob", "carol", "dave"} {
		if _, err := f.svc.Execute(ctx, f.pub("owner"), "setmember", raws(f.pub(name), map[string]any{"role": "member"})); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *roomFixture) pub(name string) string {
	pk, _ := event.PublicKey(f.keys[name])
	return pk
}

func (f *roomFixture) event(t *testing.T, who string, kind int, tags [][]string, content string) event.Event {
	t.Helper()
	e := event.Event{Kind: kind, CreatedAt: time.Now().Unix(), Tags: tags, Content: content}
	if err := event.Sign(&e, f.keys[who]); err != nil {
		t.Fatal(err)
	}
	return e
}

func (f *roomFixture) persist(e event.Event) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		_, err := storage.SaveTx(f.ctx, tx, e, storage.SaveOptions{Now: time.Now().Unix()})
		return err
	}
}

// send applies a room management event and returns the handler error.
func (f *roomFixture) send(t *testing.T, who string, kind int, tags [][]string) (MembershipResult, error) {
	t.Helper()
	e := f.event(t, who, kind, tags, "")
	return f.svc.HandleRoomEventTx(f.ctx, e, f.persist(e))
}

func (f *roomFixture) message(t *testing.T, who, room, text string) event.Event {
	t.Helper()
	e := f.event(t, who, event.KIND_CHAT, [][]string{{"h", room}}, text)
	if err := f.svc.HandleRoomMessageTx(f.ctx, e, f.persist(e)); err != nil {
		t.Fatalf("message from %s in %s: %v", who, room, err)
	}
	return e
}

func (f *roomFixture) role(t *testing.T, room, who string) string {
	t.Helper()
	role, err := f.svc.RoomRole(f.ctx, room, f.pub(who))
	if err != nil {
		t.Fatal(err)
	}
	return role
}

func TestRoomCreationHonoursMembershipAndCap(t *testing.T) {
	f := newRoomFixture(t)
	f.rooms = 1
	if _, err := f.send(t, "stranger", event.KIND_CREATE_GROUP, [][]string{{"h", "general"}, {"name", "General"}}); err == nil || !strings.HasPrefix(err.Error(), "restricted:") {
		t.Fatalf("stranger created a room: %v", err)
	}
	if _, err := f.send(t, "alice", event.KIND_CREATE_GROUP, [][]string{{"h", "Bad Room"}, {"name", "x"}}); err == nil || !strings.HasPrefix(err.Error(), "invalid:") {
		t.Fatalf("invalid id accepted: %v", err)
	}
	if _, err := f.send(t, "alice", event.KIND_CREATE_GROUP, [][]string{{"h", "main"}}); err == nil {
		t.Fatal("slug room recreated")
	}
	if _, err := f.send(t, "alice", event.KIND_CREATE_GROUP, [][]string{{"h", "general"}, {"name", "General"}, {"about", "Talk"}, {"channel_type", "public"}}); err != nil {
		t.Fatal(err)
	}
	room, err := f.svc.Room(f.ctx, "general")
	if err != nil || !room.Live() || room.Name != "General" || room.About != "Talk" || room.Access != RoomOpen || room.CreatedBy != f.pub("alice") || room.EventID == "" {
		t.Fatalf("room = %+v err=%v", room, err)
	}
	if f.role(t, "general", "alice") != RoomRoleOwner {
		t.Fatal("creator is not the room owner")
	}
	if _, err := f.send(t, "alice", event.KIND_CREATE_GROUP, [][]string{{"h", "general"}}); err == nil || !strings.HasPrefix(err.Error(), "duplicate:") {
		t.Fatalf("duplicate room: %v", err)
	}
	if _, err := f.send(t, "bob", event.KIND_CREATE_GROUP, [][]string{{"h", "second"}}); err == nil || !strings.Contains(err.Error(), "allows 1 rooms") {
		t.Fatalf("cap not applied: %v", err)
	}
	f.rooms = 0
	if _, err := f.send(t, "bob", event.KIND_CREATE_GROUP, [][]string{{"h", "third"}}); err == nil || !strings.Contains(err.Error(), "switched off") {
		t.Fatalf("rooms off not applied: %v", err)
	}
	f.rooms = 64
	if _, err := f.send(t, "bob", event.KIND_CREATE_GROUP, [][]string{{"h", "team"}, {"name", "Team"}, {"visibility", "members"}}); err != nil {
		t.Fatal(err)
	}
	if room, _ := f.svc.Room(f.ctx, "team"); room.Access != RoomMembers {
		t.Fatalf("visibility tag ignored: %+v", room)
	}
	rooms, err := f.svc.Rooms(f.ctx)
	if err != nil || len(rooms) != 3 {
		t.Fatalf("rooms = %v err=%v", rooms, err)
	}
}

func TestRoomAdminEventsFollowRoomRoles(t *testing.T) {
	f := newRoomFixture(t)
	if _, err := f.send(t, "alice", event.KIND_CREATE_GROUP, [][]string{{"h", "team"}, {"name", "Team"}, {"closed"}}); err != nil {
		t.Fatal(err)
	}
	// Metadata edits belong to room admins; the tenant owner may always moderate.
	if _, err := f.send(t, "bob", event.KIND_EDIT_METADATA, [][]string{{"h", "team"}, {"name", "Hijacked"}}); err == nil {
		t.Fatal("outsider edited the room")
	}
	if _, err := f.send(t, "owner", event.KIND_EDIT_METADATA, [][]string{{"h", "team"}, {"about", "Moderated"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.send(t, "alice", event.KIND_EDIT_METADATA, [][]string{{"h", "team"}, {"name", "Team Chat"}}); err != nil {
		t.Fatal(err)
	}
	if room, _ := f.svc.Room(f.ctx, "team"); room.Name != "Team Chat" || room.About != "Moderated" || room.Access != RoomMembers {
		t.Fatalf("edit lost: %+v", room)
	}
	// Adding people: tenant membership is required and owner appointments are reserved.
	if _, err := f.send(t, "alice", event.KIND_PUT_USER, [][]string{{"h", "team"}, {"p", f.pub("stranger")}}); err == nil {
		t.Fatal("non-member added to a room")
	}
	if _, err := f.send(t, "alice", event.KIND_PUT_USER, [][]string{{"h", "team"}, {"p", f.pub("bob"), "admin"}}); err != nil {
		t.Fatal(err)
	}
	if f.role(t, "team", "bob") != RoomRoleAdmin {
		t.Fatal("admin role not applied")
	}
	if _, err := f.send(t, "bob", event.KIND_PUT_USER, [][]string{{"h", "team"}, {"p", f.pub("carol"), "owner"}}); err == nil {
		t.Fatal("admin appointed an owner")
	}
	if _, err := f.send(t, "bob", event.KIND_PUT_USER, [][]string{{"h", "team"}, {"p", f.pub("carol")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.send(t, "carol", event.KIND_PUT_USER, [][]string{{"h", "team"}, {"p", f.pub("dave")}}); err == nil {
		t.Fatal("member added someone")
	}
	// Removal: admins cannot remove owners and the last owner stays.
	if _, err := f.send(t, "bob", event.KIND_REMOVE_USER, [][]string{{"h", "team"}, {"p", f.pub("alice")}}); err == nil {
		t.Fatal("admin removed the owner")
	}
	if _, err := f.send(t, "alice", event.KIND_LEAVE, [][]string{{"h", "team"}}); err == nil || !strings.Contains(err.Error(), "at least one owner") {
		t.Fatalf("last owner left: %v", err)
	}
	if _, err := f.send(t, "alice", event.KIND_PUT_USER, [][]string{{"h", "team"}, {"p", f.pub("bob"), "owner"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.send(t, "alice", event.KIND_LEAVE, [][]string{{"h", "team"}}); err != nil {
		t.Fatal(err)
	}
	if f.role(t, "team", "alice") != "" || f.role(t, "team", "bob") != RoomRoleOwner {
		t.Fatal("ownership hand-over failed")
	}
	if _, err := f.send(t, "bob", event.KIND_REMOVE_USER, [][]string{{"h", "team"}, {"p", f.pub("bob")}}); err == nil {
		t.Fatal("last owner removed themselves")
	}
	if _, err := f.send(t, "bob", event.KIND_REMOVE_USER, [][]string{{"h", "team"}, {"p", f.pub("carol")}}); err != nil {
		t.Fatal(err)
	}
	// Join requests: members-only rooms refuse them, open rooms take tenant members.
	if _, err := f.send(t, "carol", event.KIND_JOIN, [][]string{{"h", "team"}}); err == nil || !strings.Contains(err.Error(), "members-only") {
		t.Fatalf("joined a members-only room: %v", err)
	}
	if _, err := f.send(t, "bob", event.KIND_EDIT_METADATA, [][]string{{"h", "team"}, {"open"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.send(t, "stranger", event.KIND_JOIN, [][]string{{"h", "team"}}); err == nil {
		t.Fatal("stranger joined")
	}
	if _, err := f.send(t, "carol", event.KIND_JOIN, [][]string{{"h", "team"}}); err != nil {
		t.Fatal(err)
	}
	if res, err := f.send(t, "carol", event.KIND_JOIN, [][]string{{"h", "team"}}); err != nil || res.Stored || !strings.HasPrefix(res.Message, "duplicate:") {
		t.Fatalf("second join = %+v %v", res, err)
	}
	// Deleting events: authors and admins only, and only inside this room.
	own := f.message(t, "carol", "team", "mine")
	other := f.message(t, "bob", "team", "theirs")
	if _, err := f.send(t, "carol", event.KIND_DELETE_EVENT, [][]string{{"h", "team"}, {"e", other.ID}}); err == nil {
		t.Fatal("member deleted another member's message")
	}
	if _, err := f.send(t, "carol", event.KIND_DELETE_EVENT, [][]string{{"h", "team"}, {"e", own.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.send(t, "bob", event.KIND_DELETE_EVENT, [][]string{{"h", "team"}, {"e", other.ID}}); err != nil {
		t.Fatal(err)
	}
	var stored int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE id IN (?,?)`, own.ID, other.ID).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("messages remain: %d %v", stored, err)
	}
	// Deleting the room: owners only, never the slug room, and the history goes.
	left := f.message(t, "carol", "team", "still here")
	if _, err := f.send(t, "carol", event.KIND_DELETE_GROUP, [][]string{{"h", "team"}}); err == nil {
		t.Fatal("member deleted the room")
	}
	if _, err := f.send(t, "owner", event.KIND_DELETE_GROUP, [][]string{{"h", "main"}}); err == nil {
		t.Fatal("slug room deleted")
	}
	if _, err := f.send(t, "bob", event.KIND_DELETE_GROUP, [][]string{{"h", "team"}}); err != nil {
		t.Fatal(err)
	}
	if room, _ := f.svc.Room(f.ctx, "team"); room.Live() {
		t.Fatal("room still live")
	}
	if n, _ := f.svc.RoomMemberCount(f.ctx, "team"); n != 0 {
		t.Fatalf("members remain: %d", n)
	}
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE id=?`, left.ID).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("history remains: %d %v", stored, err)
	}
	if _, err := f.send(t, "carol", event.KIND_JOIN, [][]string{{"h", "team"}}); err == nil {
		t.Fatal("joined a deleted room")
	}
	if _, err := f.send(t, "dave", event.KIND_CREATE_GROUP, [][]string{{"h", "team"}, {"name", "Reborn"}}); err != nil {
		t.Fatalf("deleted id not reusable: %v", err)
	}
	if f.role(t, "team", "dave") != RoomRoleOwner || f.role(t, "team", "bob") != "" {
		t.Fatal("recreated room kept old members")
	}
}

func TestRoomMessagesJoinOpenRoomsAndRespectMembersOnly(t *testing.T) {
	f := newRoomFixture(t)
	if _, err := f.send(t, "alice", event.KIND_CREATE_GROUP, [][]string{{"h", "open"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.send(t, "alice", event.KIND_CREATE_GROUP, [][]string{{"h", "closed"}, {"closed"}}); err != nil {
		t.Fatal(err)
	}
	f.message(t, "bob", "open", "hello")
	if f.role(t, "open", "bob") != RoomRoleMember {
		t.Fatal("first message did not join the open room")
	}
	e := f.event(t, "bob", event.KIND_CHAT, [][]string{{"h", "closed"}}, "psst")
	if err := f.svc.HandleRoomMessageTx(f.ctx, e, f.persist(e)); err == nil || !strings.HasPrefix(err.Error(), "restricted:") {
		t.Fatalf("outsider wrote into a members-only room: %v", err)
	}
	e = f.event(t, "stranger", event.KIND_CHAT, [][]string{{"h", "open"}}, "hi")
	if err := f.svc.HandleRoomMessageTx(f.ctx, e, f.persist(e)); err == nil {
		t.Fatal("stranger wrote into an open room")
	}
	allowed, joins, err := f.svc.RoomWriteAllowed(f.ctx, "open", f.pub("carol"))
	if err != nil || !allowed || !joins {
		t.Fatalf("open room write = %v %v %v", allowed, joins, err)
	}
	if allowed, _, _ := f.svc.RoomWriteAllowed(f.ctx, "closed", f.pub("carol")); allowed {
		t.Fatal("members-only room open to tenant members")
	}
	if visible, _ := f.svc.RoomVisible(f.ctx, "closed", []string{f.pub("carol")}); visible {
		t.Fatal("members-only room visible to outsiders")
	}
	for _, who := range []string{"alice", "owner"} {
		if visible, _ := f.svc.RoomVisible(f.ctx, "closed", []string{f.pub(who)}); !visible {
			t.Fatalf("members-only room hidden from %s", who)
		}
	}
	if visible, _ := f.svc.RoomVisible(f.ctx, "open", nil); !visible {
		t.Fatal("open room hidden")
	}
}

func TestSlugRoomMirrorsTenantRoster(t *testing.T) {
	f := newRoomFixture(t)
	room, err := f.svc.Room(f.ctx, "main")
	if err != nil || !room.Live() || room.Access != RoomOpen {
		t.Fatalf("slug room = %+v %v", room, err)
	}
	if f.svc.SlugRoom() != "main" || f.role(t, "main", "owner") != RoomRoleOwner || f.role(t, "main", "alice") != RoomRoleMember {
		t.Fatal("roster not mirrored")
	}
	if _, err := f.svc.Execute(f.ctx, f.pub("owner"), "setmember", raws(f.pub("alice"), map[string]any{"role": "moderator"})); err != nil {
		t.Fatal(err)
	}
	if f.role(t, "main", "alice") != RoomRoleAdmin {
		t.Fatal("moderator is not a room admin")
	}
	if _, err := f.send(t, "bob", event.KIND_CREATE_GROUP, [][]string{{"h", "side"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Execute(f.ctx, f.pub("owner"), "removemember", raws(f.pub("bob"))); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM room_members WHERE pubkey=?`, f.pub("bob")).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("removed member still in rooms: %d %v", rows, err)
	}
	if err := f.svc.EnsureSlugRoom(f.ctx, "main", RoomMembers); err != nil {
		t.Fatal(err)
	}
	if room, _ := f.svc.Room(f.ctx, "main"); room.Access != RoomMembers {
		t.Fatal("slug access did not follow the read rule")
	}
	if _, err := f.svc.Execute(f.ctx, f.pub("owner"), "transferowner", raws(f.pub("alice"))); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetOwner(f.pub("alice")); err != nil {
		t.Fatal(err)
	}
	if f.role(t, "main", "alice") != RoomRoleOwner || f.role(t, "main", "owner") != RoomRoleAdmin {
		t.Fatalf("transfer not mirrored: alice=%s owner=%s", f.role(t, "main", "alice"), f.role(t, "main", "owner"))
	}
}

func TestSlugRoomFirstMirrorIsSilent(t *testing.T) {
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	owner := strings.Repeat("a", 64)
	svc, err := New(ctx, st, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Execute(ctx, owner, "setmember", raws(strings.Repeat("b", 64), map[string]any{"role": "member"})); err != nil {
		t.Fatal(err)
	}
	if err := svc.EnsureSlugRoom(ctx, "main", RoomOpen); err != nil {
		t.Fatal(err)
	}
	var notices int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM work_intents WHERE target='room' AND json_extract(payload,'$.pubkey') IS NOT NULL`).Scan(&notices); err != nil || notices != 0 {
		t.Fatalf("initial mirror queued %d member notices (%v)", notices, err)
	}
	if _, err := svc.Execute(ctx, owner, "setmember", raws(strings.Repeat("c", 64), map[string]any{"role": "member"})); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM work_intents WHERE target='room' AND json_extract(payload,'$.pubkey')=?`, strings.Repeat("c", 64)).Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("later join queued %d notices (%v)", notices, err)
	}
}

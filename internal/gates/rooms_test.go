package gates

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func roomGate(t *testing.T) (*Gate, *community.Service, map[string]string, policy.Policy) {
	t.Helper()
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	secrets := map[string]string{"owner": strings.Repeat("0", 63) + "1", "alice": strings.Repeat("0", 63) + "2", "bob": strings.Repeat("0", 63) + "3", "stranger": strings.Repeat("0", 63) + "4"}
	keys := map[string]string{}
	for name, secret := range secrets {
		keys[name], _ = event.PublicKey(secret)
	}
	p := policy.Defaults(keys["owner"])
	svc, err := community.New(ctx, st, keys["owner"])
	if err != nil {
		t.Fatal(err)
	}
	svc.ConfigurePolicy(func() policy.Policy { return p })
	if err := svc.EnsureSlugRoom(ctx, "main", community.RoomOpen); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "bob"} {
		if _, err := svc.Execute(ctx, keys["owner"], "setmember", []json.RawMessage{mustRaw(keys[name]), mustRaw(map[string]any{"role": "member"})}); err != nil {
			t.Fatal(err)
		}
	}
	sign := func(who string, kind int, tags [][]string) event.Event {
		e := event.Event{Kind: kind, CreatedAt: time.Now().Unix(), Tags: tags, Content: "x"}
		if err := event.Sign(&e, secrets[who]); err != nil {
			t.Fatal(err)
		}
		return e
	}
	persist := func(e event.Event) func(*sql.Tx) error {
		return func(tx *sql.Tx) error {
			_, err := storage.SaveTx(ctx, tx, e, storage.SaveOptions{Now: time.Now().Unix()})
			return err
		}
	}
	for _, room := range []struct{ id, access string }{{"pub", "open"}, {"priv", "members"}} {
		e := sign("alice", event.KIND_CREATE_GROUP, [][]string{{"h", room.id}, {"visibility", room.access}})
		if _, err := svc.HandleRoomEventTx(ctx, e, persist(e)); err != nil {
			t.Fatal(err)
		}
	}
	g, err := New(Config{Store: st, Community: svc, Policy: func() policy.Policy { return p }, Slug: "main"})
	if err != nil {
		t.Fatal(err)
	}
	keys["secret:alice"], keys["secret:bob"], keys["secret:stranger"], keys["secret:owner"] = secrets["alice"], secrets["bob"], secrets["stranger"], secrets["owner"]
	return g, svc, keys, p
}

func TestRoomWritesNeedMembership(t *testing.T) {
	g, _, keys, _ := roomGate(t)
	ctx := context.Background()
	now := time.Now().Unix()
	sign := func(who string, kind int, tags [][]string) event.Event {
		e := event.Event{Kind: kind, CreatedAt: now, Tags: tags, Content: "x"}
		if err := event.Sign(&e, keys["secret:"+who]); err != nil {
			t.Fatal(err)
		}
		return e
	}
	for _, tc := range []struct {
		name   string
		e      event.Event
		reject string
	}{
		{"unknown room", sign("alice", 9, [][]string{{"h", "nope"}}), "invalid: unknown room"},
		{"room owner posts", sign("alice", 9, [][]string{{"h", "priv"}}), ""},
		{"tenant member outside a members-only room", sign("bob", 9, [][]string{{"h", "priv"}}), "restricted:"},
		{"tenant owner moderates any room", sign("owner", 9, [][]string{{"h", "priv"}}), ""},
		{"tenant member in an open room", sign("bob", 9, [][]string{{"h", "pub"}}), ""},
		{"stranger in an open room", sign("stranger", 9, [][]string{{"h", "pub"}}), "restricted:"},
		{"reaction in an open room", sign("bob", 7, [][]string{{"h", "pub"}, {"e", strings.Repeat("a", 64)}}), ""},
		{"rich content in an open room", sign("bob", event.KIND_RICH_CONTENT, [][]string{{"h", "pub"}}), ""},
		{"presence from a member", sign("bob", event.KIND_ROOM_PRESENCE, [][]string{{"h", "pub"}}), ""},
		{"presence from a stranger", sign("stranger", event.KIND_ROOM_TYPING, [][]string{{"h", "pub"}}), "restricted:"},
		{"member notices are relay signed", sign("alice", event.KIND_ROOM_MEMBER_ADDED, [][]string{{"h", "pub"}, {"p", keys["bob"]}}), "blocked:"},
		{"room creation names a new room", sign("bob", event.KIND_CREATE_GROUP, [][]string{{"h", "fresh"}}), ""},
		{"join request reaches the room handler", sign("stranger", event.KIND_JOIN, [][]string{{"h", "pub"}}), ""},
		{"slug room keeps the open write rule", sign("stranger", 9, [][]string{{"h", "main"}}), ""},
		{"no h tag is outside rooms", sign("stranger", 9, nil), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := g.Write(ctx, tc.e, relay.Session{PubKeys: []string{tc.e.PubKey}}, now)
			if tc.reject == "" && err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if tc.reject != "" && (err == nil || !strings.HasPrefix(err.Error(), tc.reject)) {
				t.Fatalf("want %q, got %v", tc.reject, err)
			}
		})
	}
	if err := g.Import(ctx, sign("alice", 9, [][]string{{"h", "gone"}}), now); err == nil {
		t.Fatal("import accepted an unknown room")
	}
	if err := g.Import(ctx, sign("stranger", 9, [][]string{{"h", "priv"}}), now); err != nil {
		t.Fatalf("import applies room existence only: %v", err)
	}
}

func TestRoomReadsFollowRoomAccess(t *testing.T) {
	g, _, keys, _ := roomGate(t)
	ctx := context.Background()
	private := event.Event{Kind: 9, PubKey: keys["alice"], CreatedAt: 1, Tags: [][]string{{"h", "priv"}}, Content: "secret"}
	public := event.Event{Kind: 9, PubKey: keys["alice"], CreatedAt: 1, Tags: [][]string{{"h", "pub"}}, Content: "open"}
	members := event.Event{Kind: event.KIND_GROUP_MEMBERS, PubKey: strings.Repeat("f", 64), CreatedAt: 1, Tags: [][]string{{"-"}, {"d", "priv"}, {"p", keys["alice"]}}}
	main := event.Event{Kind: 9, PubKey: keys["alice"], CreatedAt: 1, Tags: [][]string{{"h", "main"}}, Content: "main"}
	for _, tc := range []struct {
		name string
		e    event.Event
		who  []string
		want bool
	}{
		{"member sees a members-only room", private, []string{keys["alice"]}, true},
		{"tenant owner sees every room", private, []string{keys["owner"]}, true},
		{"tenant member outside the room does not", private, []string{keys["bob"]}, false},
		{"anonymous does not", private, nil, false},
		{"member list of a members-only room is hidden", members, []string{keys["bob"]}, false},
		{"member list is served to members", members, []string{keys["alice"]}, true},
		{"open rooms follow the tenant read rule", public, nil, true},
		{"slug room is unchanged", main, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := g.CanSee(ctx, tc.e, relay.Session{PubKeys: tc.who}, nil); got != tc.want {
				t.Fatalf("visible = %v, want %v", got, tc.want)
			}
		})
	}
	hint, err := g.Read(ctx, []event.Filter{{Kinds: []int{9}, Tags: map[string][]string{"h": {"priv"}}}}, relay.Session{})
	if err != nil || !hint {
		t.Fatalf("members-only room read hint = %v %v", hint, err)
	}
	hint, err = g.Read(ctx, []event.Filter{{Kinds: []int{9}, Tags: map[string][]string{"h": {"pub"}}}}, relay.Session{})
	if err != nil || hint {
		t.Fatalf("open room read hint = %v %v", hint, err)
	}
}

func mustRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

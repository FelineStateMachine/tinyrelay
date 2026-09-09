package records

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
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestRoomRecordsAndNotices(t *testing.T) {
	s, ctx := testStore(t)
	secrets := map[string]string{"owner": strings.Repeat("0", 63) + "1", "alice": strings.Repeat("0", 63) + "2", "bob": strings.Repeat("0", 63) + "3", "carol": strings.Repeat("0", 63) + "4"}
	keys := map[string]string{}
	for name, secret := range secrets {
		keys[name], _ = event.PublicKey(secret)
	}
	p := policy.Defaults(keys["owner"])
	svc, err := community.New(ctx, s, keys["owner"])
	if err != nil {
		t.Fatal(err)
	}
	svc.ConfigurePolicy(func() policy.Policy { return p })
	if err := svc.EnsureSlugRoom(ctx, "main", community.RoomOpen); err != nil {
		t.Fatal(err)
	}
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	for _, name := range []string{"alice", "bob", "carol"} {
		if _, err := svc.Execute(ctx, keys["owner"], "setmember", []json.RawMessage{raw(keys[name]), raw(map[string]any{"role": "member"})}); err != nil {
			t.Fatal(err)
		}
	}
	send := func(who string, kind int, tags [][]string) {
		t.Helper()
		e := event.Event{Kind: kind, CreatedAt: time.Now().Unix(), Tags: tags}
		if err := event.Sign(&e, secrets[who]); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.HandleRoomEventTx(ctx, e, func(tx *sql.Tx) error {
			_, err := storage.SaveTx(ctx, tx, e, storage.SaveOptions{Now: time.Now().Unix()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	send("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "team"}, {"name", "Team"}, {"about", "Ours"}, {"picture", "https://img.example/t.png"}, {"closed"}})
	send("alice", event.KIND_PUT_USER, [][]string{{"h", "team"}, {"p", keys["bob"], "admin"}})
	send("alice", event.KIND_PUT_USER, [][]string{{"h", "team"}, {"p", keys["carol"]}})

	var generated []event.Event
	r, err := New(ctx, Config{Store: s, Policy: func() policy.Policy { return p }, RelayURL: "wss://relay.example", GroupID: "main", OnGenerated: func(ctx context.Context, e event.Event) error {
		generated = append(generated, e)
		_, err := s.Save(ctx, e, storage.SaveOptions{Now: e.CreatedAt})
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	records, err := r.PublishRoom(ctx, "team", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || records[0].Kind != event.KIND_GROUP_METADATA || records[1].Kind != event.KIND_GROUP_ADMINS || records[2].Kind != event.KIND_GROUP_MEMBERS {
		t.Fatalf("records = %+v", records)
	}
	meta := records[0]
	if event.Tag(meta, "d") != "team" || event.Tag(meta, "name") != "Team" || event.Tag(meta, "about") != "Ours" || event.Tag(meta, "picture") != "https://img.example/t.png" {
		t.Fatalf("metadata tags = %v", meta.Tags)
	}
	if !hasBareTag(meta, "closed") || hasBareTag(meta, "private") || !hasBareTag(meta, "public") {
		t.Fatalf("access tags = %v", meta.Tags)
	}
	admins := map[string]string{}
	for _, tag := range records[1].Tags {
		if tag[0] == "p" {
			admins[tag[1]] = tag[2]
		}
	}
	if len(admins) != 2 || admins[keys["alice"]] != "owner" || admins[keys["bob"]] != "admin" {
		t.Fatalf("admins = %v", admins)
	}
	if got := event.TagValues(records[2], "p"); len(got) != 3 {
		t.Fatalf("members = %v", got)
	}
	for _, rec := range records {
		if rec.PubKey != r.PublicKey() || event.Validate(rec) != nil {
			t.Fatalf("record not relay-signed: %+v", rec)
		}
	}
	p.Reads = "members"
	records, err = r.PublishRoom(ctx, "team", 101)
	if err != nil || !hasBareTag(records[0], "private") {
		t.Fatalf("private marker missing: %v %v", records[0].Tags, err)
	}
	notice, err := r.RoomNotice(ctx, "team", keys["carol"], true, 102)
	if err != nil || notice.Kind != event.KIND_ROOM_MEMBER_ADDED || event.Tag(notice, "h") != "team" || event.Tag(notice, "p") != keys["carol"] {
		t.Fatalf("added notice = %+v %v", notice, err)
	}
	notice, err = r.RoomNotice(ctx, "team", keys["carol"], false, 103)
	if err != nil || notice.Kind != event.KIND_ROOM_MEMBER_REMOVED {
		t.Fatalf("removed notice = %+v %v", notice, err)
	}
	stored, err := s.Query(ctx, event.Filter{Kinds: []int{event.KIND_GROUP_METADATA, event.KIND_GROUP_ADMINS, event.KIND_GROUP_MEMBERS}, Authors: []string{r.PublicKey()}, Tags: map[string][]string{"d": {"team"}}}, storage.QueryOptions{Access: storage.Access{All: true}})
	if err != nil || len(stored.Events) != 3 {
		t.Fatalf("stored records = %d %v", len(stored.Events), err)
	}
	if err := r.RetireRoom(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	stored, err = s.Query(ctx, event.Filter{Kinds: []int{event.KIND_GROUP_METADATA}, Authors: []string{r.PublicKey()}, Tags: map[string][]string{"d": {"team"}}}, storage.QueryOptions{Access: storage.Access{All: true}})
	if err != nil || len(stored.Events) != 0 {
		t.Fatalf("retired records remain: %d %v", len(stored.Events), err)
	}
	if len(generated) < 8 {
		t.Fatalf("generated %d events", len(generated))
	}
}

func hasBareTag(e event.Event, name string) bool {
	for _, tag := range e.Tags {
		if len(tag) == 1 && tag[0] == name {
			return true
		}
	}
	return false
}

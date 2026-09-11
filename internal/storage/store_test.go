package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func sampleEvent(t *testing.T, kind int, at int64, tags [][]string) event.Event {
	t.Helper()
	e := event.Event{Kind: kind, CreatedAt: at, Tags: tags, Content: "hello storage"}
	if e.Tags == nil {
		e.Tags = [][]string{}
	}
	if err := event.Sign(&e, "0000000000000000000000000000000000000000000000000000000000000001"); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestDurableAcceptanceIncludesIntents(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	e := sampleEvent(t, 1, 100, nil)
	opts := SaveOptions{Now: 101, Intents: []Intent{{Kind: "delivery", EventID: e.ID, Target: "wss://example.com", Payload: `{}`}}}
	if _, err := s.Save(ctx, e, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, e, opts); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
	var n int
	if err := s.DB().QueryRowContext(ctx, "SELECT count(*) FROM work_intents").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("got %d intents", n)
	}
	got, err := s.Query(ctx, event.Filter{}, QueryOptions{Now: 101})
	if err != nil || len(got.Events) != 1 {
		t.Fatalf("query: %+v %v", got, err)
	}
}

func TestDeletionArchivesAgentGrant(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	agent := "0000000000000000000000000000000000000000000000000000000000000002"
	grant := sampleEvent(t, event.KIND_AGENT_GRANT, 100, [][]string{{"d", agent}, {"p", agent}, {"expiration", "200"}})
	if _, err := s.Save(ctx, grant, SaveOptions{Now: 100}); err != nil {
		t.Fatal(err)
	}
	del := sampleEvent(t, event.KIND_DELETION, 110, [][]string{{"e", grant.ID}})
	del.PubKey = grant.PubKey
	if err := event.Sign(&del, "0000000000000000000000000000000000000000000000000000000000000001"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, del, SaveOptions{Now: 110}); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.DB().QueryRowContext(ctx, "SELECT raw FROM agent_grant_revisions WHERE event_id=?", grant.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "" {
		t.Fatal("deleted grant revision is empty")
	}
}

func TestEventAndIntentRollbackTogether(t *testing.T) {
	s := openTestStore(t)
	e := sampleEvent(t, 1, 100, nil)
	opts := SaveOptions{Now: 101, BeforeCommit: func(context.Context, *sql.Tx) error { return errors.New("simulated disk failure") }}
	if _, err := s.Save(context.Background(), e, opts); err == nil {
		t.Fatal("expected failure")
	}
	got, err := s.Query(context.Background(), event.Filter{}, QueryOptions{Now: 101})
	if err != nil || len(got.Events) != 0 {
		t.Fatalf("uncommitted event visible: %+v %v", got, err)
	}
}

func TestReplaceDeleteExpiryAndPrivacy(t *testing.T) {
	ctx := context.Background()
	t.Run("replacement and history", func(t *testing.T) {
		s := openTestStore(t)
		old := sampleEvent(t, 3, 100, [][]string{{"p", "old"}})
		fresh := sampleEvent(t, 3, 102, [][]string{{"p", "new"}})
		for _, e := range []event.Event{old, fresh} {
			if _, err := s.Save(ctx, e, SaveOptions{Now: 103}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Save(ctx, old, SaveOptions{Now: 103}); !errors.Is(err, ErrReplaced) {
			t.Fatalf("got %v", err)
		}
		got, err := s.Query(ctx, event.Filter{}, QueryOptions{Now: 103})
		if err != nil || len(got.Events) != 1 || got.Events[0].ID != fresh.ID {
			t.Fatalf("replace: %+v %v", got, err)
		}
		rows, err := s.ListHistory(ctx, old.PubKey, 103)
		if err != nil || len(rows) != 1 {
			t.Fatalf("history: %+v %v", rows, err)
		}
	})
	t.Run("deletion arrives first", func(t *testing.T) {
		s := openTestStore(t)
		e := sampleEvent(t, 1, 100, nil)
		d := sampleEvent(t, 5, 102, [][]string{{"e", e.ID}})
		if _, err := s.Save(ctx, d, SaveOptions{Now: 103}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Save(ctx, e, SaveOptions{Now: 103}); !errors.Is(err, ErrDeleted) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("private count and query agree", func(t *testing.T) {
		s := openTestStore(t)
		e := sampleEvent(t, 1059, 100, [][]string{{"p", "recipient"}})
		if _, err := s.Save(ctx, e, SaveOptions{Now: 101}); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			who  Access
			want int
		}{{Access{}, 0}, {Access{PubKeys: []string{"recipient"}}, 1}, {Access{PubKeys: []string{"stranger"}}, 0}} {
			opts := QueryOptions{Now: 101, Access: tc.who}
			got, err := s.Query(ctx, event.Filter{}, opts)
			if err != nil || len(got.Events) != tc.want {
				t.Fatalf("privacy: %+v %v", got, err)
			}
			n, err := s.Count(ctx, []event.Filter{{}}, opts)
			if err != nil || n != int64(tc.want) {
				t.Fatalf("count: %d %v", n, err)
			}
		}
	})
	t.Run("nip59 seals are visible only to their author", func(t *testing.T) {
		s := openTestStore(t)
		e := sampleEvent(t, 13, 100, [][]string{})
		if _, err := s.Save(ctx, e, SaveOptions{Now: 101}); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			who  Access
			want int
		}{{Access{}, 0}, {Access{PubKeys: []string{e.PubKey}}, 1}, {Access{PubKeys: []string{"stranger"}}, 0}} {
			got, err := s.Query(ctx, event.Filter{Kinds: []int{13}}, QueryOptions{Now: 101, Access: tc.who})
			if err != nil || len(got.Events) != tc.want {
				t.Fatalf("seal query: %+v %v", got, err)
			}
			n, err := s.Count(ctx, []event.Filter{{Kinds: []int{13}}}, QueryOptions{Now: 101, Access: tc.who})
			if err != nil || n != int64(tc.want) {
				t.Fatalf("seal count: %d %v", n, err)
			}
		}
	})
	t.Run("expired and ephemeral", func(t *testing.T) {
		s := openTestStore(t)
		for _, e := range []event.Event{sampleEvent(t, 1, 100, [][]string{{"expiration", "102"}}), sampleEvent(t, 20001, 100, nil)} {
			if _, err := s.Save(ctx, e, SaveOptions{Now: 101}); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.Query(ctx, event.Filter{}, QueryOptions{Now: 103})
		if err != nil || len(got.Events) != 0 {
			t.Fatalf("expiration: %+v %v", got, err)
		}
	})
}

func TestEmptyFilterArraysMatchNothing(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.Save(context.Background(), sampleEvent(t, 1, 100, nil), SaveOptions{Now: 101}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []event.Filter{{IDs: []string{}}, {Authors: []string{}}, {Kinds: []int{}}, {Tags: map[string][]string{"p": {}}}} {
		got, err := s.Query(context.Background(), f, QueryOptions{Now: 101})
		if err != nil || len(got.Events) != 0 {
			t.Fatalf("filter: %+v %v", got, err)
		}
	}
}

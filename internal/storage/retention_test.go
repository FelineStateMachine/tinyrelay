package storage

import (
	"context"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestRetentionPreservesIdentityListsAndMail(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	for _, kind := range []int{1, 0, 3, 30023, 1059, 9735} {
		e := sampleEvent(t, kind, 100, [][]string{{"d", "test"}})
		if _, err := s.Save(ctx, e, SaveOptions{Now: 101}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.SweepRetention(ctx, RetentionOptions{Now: 200000, Rules: []RetentionRule{{Kind: -1, Days: 1}}})
	if err != nil || n != 1 {
		t.Fatalf("sweep %d %v", n, err)
	}
	rows, err := s.Query(ctx, event.Filter{}, QueryOptions{Now: 200000, Access: Access{All: true}})
	if err != nil || len(rows.Events) != 5 {
		t.Fatalf("retained %d %v", len(rows.Events), err)
	}
	n, err = s.SweepRetention(ctx, RetentionOptions{Now: 200000, Rules: []RetentionRule{{Kind: 1059, Days: 1}}})
	if err != nil || n != 1 {
		t.Fatalf("explicit mail rule %d %v", n, err)
	}
}

func TestRetentionSpecificRuleOverridesCatchAllAndKeepsRelayRecords(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	e := sampleEvent(t, 1, 100, nil)
	if _, err := s.Save(ctx, e, SaveOptions{Now: 101}); err != nil {
		t.Fatal(err)
	}
	opts := RetentionOptions{Now: 200000, Identity: e.PubKey, Rules: []RetentionRule{{Kind: -1, Days: 1}, {Kind: 1, Days: 1}}}
	n, err := s.SweepRetention(ctx, opts)
	if err != nil || n != 0 {
		t.Fatalf("relay records %d %v", n, err)
	}
	opts.Identity = ""
	opts.Rules[1].Days = 7
	n, err = s.SweepRetention(ctx, opts)
	if err != nil || n != 0 {
		t.Fatalf("specific longer rule %d %v", n, err)
	}
}

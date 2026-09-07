package gates

import (
	"context"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
)

func TestImportDoesNotApplyClientWritePolicy(t *testing.T) {
	p := policy.Defaults(strings.Repeat("a", 64))
	p.Writes = "owner"
	g, err := New(Config{Policy: func() policy.Policy { return p }})
	if err != nil {
		t.Fatal(err)
	}
	e := event.Event{CreatedAt: 10, Kind: 1, Tags: [][]string{}, Content: "pulled"}
	if err := event.Sign(&e, strings.Repeat("1", 64)); err != nil {
		t.Fatal(err)
	}
	if err := g.Import(context.Background(), e, 10); err != nil {
		t.Fatalf("import should skip client write policy: %v", err)
	}
	if err := g.Write(context.Background(), e, relay.Session{}, 10); err == nil {
		t.Fatal("client write unexpectedly accepted")
	}
}

func TestNIP43RequestsRequireProtectedShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind int
		tags [][]string
	}{
		{name: "join claim", kind: event.KIND_NIP43_JOIN, tags: [][]string{{"-"}, {"claim", "code"}}},
		{name: "leave", kind: event.KIND_NIP43_LEAVE, tags: [][]string{{"-"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := event.Event{CreatedAt: 10, Kind: tc.kind, Tags: tc.tags}
			if err := event.Sign(&e, strings.Repeat("1", 64)); err != nil {
				t.Fatal(err)
			}
			if err := nip43Shape(e, 10); err != nil {
				t.Fatalf("valid request rejected: %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		kind int
		tags [][]string
	}{
		{name: "join without claim", kind: event.KIND_NIP43_JOIN, tags: [][]string{{"-"}}},
		{name: "leave without protection", kind: event.KIND_NIP43_LEAVE, tags: [][]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := event.Event{CreatedAt: 10, Kind: tc.kind, Tags: tc.tags}
			if err := event.Sign(&e, strings.Repeat("1", 64)); err != nil {
				t.Fatal(err)
			}
			if err := nip43Shape(e, 10); err == nil {
				t.Fatal("malformed request accepted")
			}
		})
	}
}

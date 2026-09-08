package daemon

import (
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestCollaborationTitleAndSearch(t *testing.T) {
	cases := []struct {
		name  string
		event event.Event
		want  string
	}{
		{name: "subject tag", event: event.Event{Tags: [][]string{{"subject", "Fix login"}}, Content: "details"}, want: "Fix login"},
		{name: "json title", event: event.Event{Content: `{"title":"Add files"}`}, want: "Add files"},
		{name: "first line", event: event.Event{Content: "Improve docs\nMore details"}, want: "Improve docs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := collaborationTitle(tc.event); got != tc.want {
				t.Fatalf("title = %q, want %q", got, tc.want)
			}
		})
	}
	if !collaborationMatches(event.Event{Tags: [][]string{{"t", "security"}}, Content: "please review"}, "SEC") {
		t.Fatal("label search did not match")
	}
}

func TestStatusFromEventsUsesNewestEvent(t *testing.T) {
	rows := []event.Event{{ID: "b", Kind: 1630, CreatedAt: 10}, {ID: "a", Kind: 1632, CreatedAt: 11}, {ID: "c", Kind: 1633, CreatedAt: 11}}
	if got := statusFromEvents(event.Event{}, rows); got != "closed" {
		t.Fatalf("status = %q, want closed", got)
	}
}

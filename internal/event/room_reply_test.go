package event

import (
	"strings"
	"testing"
)

func TestRoomReplyRoot(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, test := range []struct {
		name string
		kind int
		tags [][]string
		want string
	}{
		{"root and parent", 9, [][]string{{"e", b, "", "reply", b}, {"e", a, "", "root", a}}, a},
		{"parent only", 9, [][]string{{"e", b, "", "reply"}}, b},
		{"mention is not reply", 9, [][]string{{"e", a, "", "mention"}}, ""},
		{"quote is not reply", 9, [][]string{{"q", a}}, ""},
		{"legacy", 12, [][]string{{"e", a}}, a},
		{"unmarked", 9, [][]string{{"e", a}, {"e", b}}, a},
		{"reaction", 7, [][]string{{"e", a}}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := RoomReplyRoot(Event{Kind: test.kind, Tags: test.tags}); got != test.want {
				t.Fatalf("got %q want %q", got, test.want)
			}
		})
	}
}

package daemon

import (
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestSocialReactionValueNIP30(t *testing.T) {
	url := "https://cdn.example/percent.png"
	reaction := event.Event{Content: ":100percent:", Tags: [][]string{{"emoji", "100percent", url}}}
	content, image := socialReactionValue(reaction)
	if content != ":100percent:" || image != url {
		t.Fatalf("custom emoji = %q, %q", content, image)
	}

	for _, invalid := range []event.Event{
		{Content: ":100percent:", Tags: [][]string{{"emoji", "100percent", "javascript:alert(1)"}}},
		{Content: ":100.percent:", Tags: [][]string{{"emoji", "100.percent", url}}},
		{Content: ":unknown:", Tags: [][]string{{"emoji", "other", url}}},
	} {
		content, image := socialReactionValue(invalid)
		if image != "" || content == "" {
			t.Fatalf("invalid custom emoji = %q, %q", content, image)
		}
	}
}

func TestSocialReactionsGroupCustomEmojiByImage(t *testing.T) {
	secret := strings.Repeat("1", 63) + "2"
	other := strings.Repeat("3", 63) + "4"
	third := strings.Repeat("5", 63) + "6"
	actor, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	post := signedEvent(t, secret, 1, 100, nil, "post")
	firstURL := "https://cdn.example/one.png"
	secondURL := "https://cdn.example/two.png"
	extra := []event.Event{
		signedEvent(t, other, 7, 101, [][]string{{"e", post.ID}, {"emoji", "party", firstURL}}, ":party:"),
		signedEvent(t, other, 7, 102, [][]string{{"e", post.ID}, {"emoji", "party", firstURL}}, ":party:"),
		signedEvent(t, third, 7, 102, [][]string{{"e", post.ID}, {"emoji", "party", firstURL}}, ":party:"),
		signedEvent(t, secret, 7, 103, [][]string{{"e", post.ID}, {"emoji", "party", secondURL}}, ":party:"),
	}
	items := socialItemsWithRefs([]event.Event{post}, extra, actor, nil)
	if len(items) != 1 || len(items[0].Reactions) != 2 {
		t.Fatalf("reactions = %#v", items)
	}
	if items[0].Reactions[0].Image != firstURL || items[0].Reactions[0].Count != 2 || items[0].Reactions[0].Reacted {
		t.Fatalf("first custom reaction = %#v", items[0].Reactions[0])
	}
	if items[0].Reactions[1].Image != secondURL || items[0].Reactions[1].Count != 1 || !items[0].Reactions[1].Reacted {
		t.Fatalf("second custom reaction = %#v", items[0].Reactions[1])
	}
}

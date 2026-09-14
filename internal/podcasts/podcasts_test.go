package podcasts

import (
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestParseEpisodeUsesNativeAudioTracks(t *testing.T) {
	e := event.Event{ID: "episode", PubKey: strings.Repeat("a", 64), Kind: 54, CreatedAt: 123, Content: "## Show notes", Tags: [][]string{
		{"title", "Episode one"}, {"description", "A conversation"}, {"image", "https://example.test/cover.png"},
		{"audio", "javascript:alert(1)"}, {"audio", "https://example.test/one", "audio/mpeg"},
		{"audio", "https://example.test/one", "audio/mpeg"}, {"audio", "https://example.test/one.ogg"},
		{"imeta", "url https://example.test/one", "size 1234", "m audio/mpeg"},
	}}
	got, ok := ParseEpisode(e)
	if !ok || got.Title != "Episode one" || got.Description != "A conversation" || got.Content != e.Content || len(got.Audio) != 2 {
		t.Fatalf("episode: %#v, %v", got, ok)
	}
	if got.Audio[0].Length != 1234 || got.Audio[0].MIME != "audio/mpeg" || got.Audio[1].MIME != "audio/ogg" {
		t.Fatalf("audio: %#v", got.Audio)
	}
	for _, kind := range []int{1, 30023} {
		e.Kind = kind
		if _, ok := ParseEpisode(e); ok {
			t.Fatalf("kind %d treated as native podcast", kind)
		}
	}
	e.Kind, e.Tags = 54, [][]string{{"imeta", "url https://example.test/one.mp3", "m audio/mpeg"}}
	if _, ok := ParseEpisode(e); ok {
		t.Fatal("kind 54 without an F4 audio track treated as podcast")
	}
}

func TestAudioMetadataIsOptional(t *testing.T) {
	e := event.Event{Kind: 54, Tags: [][]string{{"audio", "https://example.test/audio"}, {"imeta", "url https://example.test/audio", "size -20"}}}
	got, ok := ParseEpisode(e)
	if !ok || got.Audio[0].Length != 0 || got.Audio[0].MIME != "application/octet-stream" {
		t.Fatalf("optional metadata: %#v, %v", got, ok)
	}
	e.Tags = append(e.Tags, []string{"audio", "https://example.test/audio.mp3", "video/mp4"})
	got, _ = ParseEpisode(e)
	if len(got.Audio) != 1 {
		t.Fatal("non-audio declared type accepted")
	}
}

func TestShowsUseLatestMetadataAndReciprocalAuthorship(t *testing.T) {
	show, host, fake := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	rows := []event.Event{
		{ID: "z", Kind: 10154, PubKey: show, CreatedAt: 20, Tags: [][]string{{"title", "Latest"}, {"image", "https://example.test/cover.jpg"}, {"website", "https://example.test"}, {"p", host, "host"}, {"p", fake, "cohost"}}},
		{ID: "old", Kind: 10154, PubKey: show, CreatedAt: 10, Tags: [][]string{{"title", "Old"}}},
		{ID: "host", Kind: 10064, PubKey: host, CreatedAt: 10, Tags: [][]string{{"p", show}}},
		{ID: "fake-old", Kind: 10064, PubKey: fake, CreatedAt: 10, Tags: [][]string{{"p", show}}},
		{ID: "fake-new", Kind: 10064, PubKey: fake, CreatedAt: 20},
	}
	got := Shows(rows)[show]
	if got.Title != "Latest" || len(got.Websites) != 1 || len(got.Authors) != 1 || got.Authors[0].Pubkey != host || got.Authors[0].Role != "host" {
		t.Fatalf("show: %#v", got)
	}
	rows = append(rows, event.Event{ID: "a", Kind: 10154, PubKey: show, CreatedAt: 20, Tags: [][]string{{"title", "Tie winner"}}})
	if got := Shows(rows)[show]; got.Title != "Tie winner" || len(got.Authors) != 0 {
		t.Fatalf("replacement tie: %#v", got)
	}
}

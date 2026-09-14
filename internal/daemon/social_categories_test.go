package daemon

import (
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestSocialCategoriesRespectMediaMetadataAndURLs(t *testing.T) {
	for _, tc := range []struct {
		name, category string
		row            event.Event
		want           bool
	}{
		{
			name: "extensionless image tag", category: "photos",
			row: event.Event{Kind: 1, Tags: [][]string{{"image", "https://cdn.example/photo"}}}, want: true,
		},
		{
			name: "mime wins over extension", category: "photos",
			row: event.Event{Kind: 1, Tags: [][]string{{"imeta", "url https://cdn.example/audio.mp3", "m image/jpeg"}}}, want: true,
		},
		{
			name: "mime wins over extension", category: "podcasts",
			row: event.Event{Kind: 1, Tags: [][]string{{"imeta", "url https://cdn.example/photo.jpg", "m audio/mpeg"}}}, want: true,
		},
		{
			name: "metadata URL does not regain extension family", category: "podcasts",
			row: event.Event{Kind: 1, Content: "https://cdn.example/audio.mp3", Tags: [][]string{{"imeta", "url https://cdn.example/audio.mp3", "m image/jpeg"}}}, want: false,
		},
		{
			name: "cover URL keeps image family", category: "podcasts",
			row: event.Event{Kind: 30023, Content: "https://cdn.example/cover.mp3", Tags: [][]string{{"image", "https://cdn.example/cover.mp3"}}}, want: false,
		},
		{
			name: "invalid imeta URL", category: "photos",
			row: event.Event{Kind: 1, Tags: [][]string{{"imeta", "url https://user:pass@cdn.example/photo.jpg", "m image/jpeg"}}}, want: false,
		},
		{
			name: "extensionless article markdown image", category: "photos",
			row: event.Event{Kind: 30023, Content: "![cover](https://cdn.example/photo)"}, want: true,
		},
		{
			name: "fenced URL ignored", category: "podcasts",
			row: event.Event{Kind: 1, Content: "```\nhttps://cdn.example/episode.mp3\n```"}, want: false,
		},
		{
			name: "inline code URL ignored", category: "podcasts",
			row: event.Event{Kind: 1, Content: "Use `https://cdn.example/episode.mp3` as an example."}, want: false,
		},
	} {
		t.Run(tc.name+"/"+tc.category, func(t *testing.T) {
			if got := socialCategoryMatches(tc.row, tc.category); got != tc.want {
				t.Fatalf("category match = %v, want %v", got, tc.want)
			}
		})
	}
}

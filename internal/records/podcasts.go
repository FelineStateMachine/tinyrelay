package records

import (
	"encoding/json"
	"sort"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/podcasts"
)

type podcastViewShow struct {
	ID          string            `json:"id,omitempty"`
	Pubkey      string            `json:"pubkey"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Image       string            `json:"image,omitempty"`
	Websites    []string          `json:"websites,omitempty"`
	Authors     []podcasts.Author `json:"authors,omitempty"`
}

type podcastViewEpisode struct {
	ID          string           `json:"id"`
	Pubkey      string           `json:"pubkey"`
	CreatedAt   int64            `json:"created_at"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	Image       string           `json:"image,omitempty"`
	Audio       []podcasts.Audio `json:"audio,omitempty"`
}

// foldPodcasts creates the compact, signed summary used by the built-in
// podcast view. The full source events remain available to Nostr clients; the
// view only carries enough information for a relay directory or RSS adapter
// to find the current shows and episodes.
func foldPodcasts(events []event.Event) ([][]string, string) {
	shows := podcasts.Shows(events)
	episodes := make([]podcasts.Episode, 0)
	for _, row := range events {
		if episode, ok := podcasts.ParseEpisode(row); ok {
			episodes = append(episodes, episode)
		}
	}
	sort.Slice(episodes, func(i, j int) bool {
		if episodes[i].CreatedAt != episodes[j].CreatedAt {
			return episodes[i].CreatedAt > episodes[j].CreatedAt
		}
		return episodes[i].ID < episodes[j].ID
	})
	if len(episodes) > 100 {
		episodes = episodes[:100]
	}
	showKeys := make([]string, 0, len(shows))
	for key := range shows {
		showKeys = append(showKeys, key)
	}
	sort.Strings(showKeys)
	if len(showKeys) > 100 {
		showKeys = showKeys[:100]
	}
	tags := make([][]string, 0, len(showKeys)+len(episodes))
	viewShows := make([]podcastViewShow, 0, len(showKeys))
	for _, key := range showKeys {
		show := shows[key]
		tags = append(tags, []string{"p", show.Pubkey, show.Title, show.Image})
		viewShows = append(viewShows, podcastViewShow{ID: show.ID, Pubkey: show.Pubkey, Title: show.Title, Description: show.Description, Image: show.Image, Websites: append([]string(nil), show.Websites...), Authors: append([]podcasts.Author(nil), show.Authors...)})
	}
	viewEpisodes := make([]podcastViewEpisode, 0, len(episodes))
	for _, episode := range episodes {
		audio := ""
		if len(episode.Audio) > 0 {
			audio = episode.Audio[0].URL
		}
		tags = append(tags, []string{"e", episode.ID, episode.Pubkey, episode.Title, audio})
		viewEpisodes = append(viewEpisodes, podcastViewEpisode{ID: episode.ID, Pubkey: episode.Pubkey, CreatedAt: episode.CreatedAt, Title: episode.Title, Description: episode.Description, Image: episode.Image, Audio: append([]podcasts.Audio(nil), episode.Audio...)})
	}
	content, _ := json.Marshal(struct {
		Shows    []podcastViewShow    `json:"shows"`
		Episodes []podcastViewEpisode `json:"episodes"`
	}{Shows: viewShows, Episodes: viewEpisodes})
	return tags, string(content)
}

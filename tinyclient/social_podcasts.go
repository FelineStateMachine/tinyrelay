package tinyclient

import (
	"encoding/json"
	"html/template"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/podcasts"
)

func socialPodcastEvent(row map[string]any) event.Event {
	if nested := row["event"]; nested != nil {
		var native event.Event
		if raw, err := json.Marshal(nested); err == nil && json.Unmarshal(raw, &native) == nil {
			return native
		}
	}
	return event.Event{ID: plainString(row["id"]), PubKey: plainString(row["pubkey"]),
		Kind: int(unixSeconds(row["kind"])), CreatedAt: unixSeconds(row["created_at"]),
		Tags: roomTags(row), Content: plainString(row["content"])}
}

func podcastEpisode(value any) podcasts.Episode {
	episode, _ := podcasts.ParseEpisode(socialPodcastEvent(valueMap(value)))
	return episode
}

func (a *App) podcastNotes(value any) template.HTML {
	return renderMarkdownWith(plainString(valueMap(value)["content"]), a.blocks())
}

func podcastProfile(profile, show any) map[string]any {
	metadata := valueMap(show)
	if plainString(metadata["id"]) == "" {
		return valueMap(profile)
	}
	result := map[string]any{"name": metadata["title"], "picture": metadata["image"], "about": metadata["description"]}
	if sites := stringValues(metadata["websites"]); len(sites) > 0 {
		result["website"] = sites[0]
	}
	return result
}

// Package podcasts reads the native show and episode events defined by NIP-F4.
package podcasts

import (
	"mime"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

const (
	KindEpisode   = 54
	KindMetadata  = 10154
	KindAuthored  = 10064
	KindFavorites = 10054
)

type Audio struct {
	URL    string `json:"url"`
	MIME   string `json:"mime"`
	Length int64  `json:"length"`
}

type Episode struct {
	ID          string  `json:"id"`
	Pubkey      string  `json:"pubkey"`
	CreatedAt   int64   `json:"created_at"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Image       string  `json:"image,omitempty"`
	Content     string  `json:"content"`
	Audio       []Audio `json:"audio"`
}

type Author struct {
	Pubkey string `json:"pubkey"`
	Role   string `json:"role,omitempty"`
}

type Show struct {
	ID          string     `json:"id,omitempty"`
	Pubkey      string     `json:"pubkey"`
	Tags        [][]string `json:"tags,omitempty"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Image       string     `json:"image,omitempty"`
	Websites    []string   `json:"websites,omitempty"`
	Authors     []Author   `json:"authors,omitempty"`
}

// ParseEpisode keeps native podcast audio separate from audio attached to notes.
// NIP-92 metadata may supplement an audio tag with a size or media type.
func ParseEpisode(e event.Event) (Episode, bool) {
	if e.Kind != KindEpisode || event.Tag(e, "h") != "" {
		return Episode{}, false
	}
	result := Episode{ID: e.ID, Pubkey: e.PubKey, CreatedAt: e.CreatedAt,
		Title: event.Tag(e, "title"), Description: event.Tag(e, "description"),
		Image: HTTPURL(event.Tag(e, "image")), Content: e.Content}
	metadata, seen := audioMetadata(e), map[string]bool{}
	for _, tag := range e.Tags {
		if len(tag) < 2 || tag[0] != "audio" {
			continue
		}
		raw := HTTPURL(tag[1])
		if raw == "" || seen[raw] {
			continue
		}
		audio := metadata[raw]
		audio.URL = raw
		if len(tag) > 2 && strings.TrimSpace(tag[2]) != "" {
			audio.MIME = tag[2]
		}
		audio.MIME = AudioMIME(raw, audio.MIME)
		if audio.MIME == "" {
			continue
		}
		seen[raw] = true
		result.Audio = append(result.Audio, audio)
	}
	return result, len(result.Audio) > 0
}

func audioMetadata(e event.Event) map[string]Audio {
	metadata := map[string]Audio{}
	for _, tag := range e.Tags {
		if len(tag) < 2 || tag[0] != "imeta" {
			continue
		}
		audio := Audio{}
		for _, field := range tag[1:] {
			name, value, _ := strings.Cut(field, " ")
			switch name {
			case "url":
				audio.URL = HTTPURL(value)
			case "m":
				audio.MIME = value
			case "size":
				if size, err := strconv.ParseInt(value, 10, 64); err == nil && size >= 0 {
					audio.Length = size
				}
			}
		}
		if audio.URL != "" {
			metadata[audio.URL] = audio
		}
	}
	return metadata
}

// AudioMIME retains explicit audio types, otherwise inferring familiar formats.
// An opaque URL with no type remains a generic enclosure for the player to read.
func AudioMIME(raw, declared string) string {
	if strings.TrimSpace(declared) != "" {
		typ, _, err := mime.ParseMediaType(strings.TrimSpace(declared))
		if err != nil {
			return ""
		}
		if typ == "application/ogg" {
			return "audio/ogg"
		}
		if strings.HasPrefix(typ, "audio/") || typ == "application/octet-stream" {
			return typ
		}
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	switch strings.ToLower(path.Ext(parsed.Path)) {
	case ".mp3":
		return "audio/mpeg"
	case ".m4a", ".mp4":
		return "audio/mp4"
	case ".ogg", ".oga", ".opus":
		return "audio/ogg"
	case ".wav":
		return "audio/wav"
	case ".flac":
		return "audio/flac"
	case ".aac":
		return "audio/aac"
	default:
		return "application/octet-stream"
	}
}

func HTTPURL(raw string) string {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Hostname() == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return ""
	}
	return raw
}

// Shows folds replaceable show metadata and checks the latest reciprocal author
// list. NIP-51 assigns authored podcasts kind 10064, also used in F4's example.
func Shows(events []event.Event) map[string]Show {
	metadata, authored := latest(events, KindMetadata), latest(events, KindAuthored)
	out := make(map[string]Show, len(metadata))
	for key, row := range metadata {
		show := Show{ID: row.ID, Pubkey: key, Tags: row.Tags, Title: event.Tag(row, "title"),
			Description: event.Tag(row, "description"), Image: HTTPURL(event.Tag(row, "image"))}
		seen := map[string]bool{}
		for _, tag := range row.Tags {
			if len(tag) < 2 {
				continue
			}
			if tag[0] == "website" {
				if raw := HTTPURL(tag[1]); raw != "" && !seen[raw] {
					show.Websites = append(show.Websites, raw)
					seen[raw] = true
				}
			}
			if tag[0] == "p" && !seen[tag[1]] && listsShow(authored[tag[1]], key) {
				role := ""
				if len(tag) > 2 {
					role = tag[2]
				}
				if role == "" || role == "host" || role == "cohost" || role == "editor" {
					show.Authors = append(show.Authors, Author{Pubkey: tag[1], Role: role})
					seen[tag[1]] = true
				}
			}
		}
		out[key] = show
	}
	return out
}

func latest(events []event.Event, kind int) map[string]event.Event {
	out := map[string]event.Event{}
	for _, row := range events {
		if row.Kind != kind || event.Tag(row, "h") != "" {
			continue
		}
		old, exists := out[row.PubKey]
		if !exists || row.CreatedAt > old.CreatedAt || row.CreatedAt == old.CreatedAt && row.ID < old.ID {
			out[row.PubKey] = row
		}
	}
	return out
}

func listsShow(list event.Event, show string) bool {
	for _, tag := range list.Tags {
		if len(tag) > 1 && tag[0] == "p" && tag[1] == show {
			return true
		}
	}
	return false
}

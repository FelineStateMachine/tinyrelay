package daemon

import (
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/views"
)

var socialContentURL = regexp.MustCompile(`https?://[^\s)<>\]"']+`)
var socialMarkdownImage = regexp.MustCompile(`!\[[^\]]*\]\((https?://[^)\s]+)\)`)

// socialCategoryMatches applies the categories used by the Social navigation
// to the same root notes and long-form posts as the main feed. Media is read
// from NIP-92 metadata first, with URL extensions retained for older events.
func socialCategoryMatches(row event.Event, category string) bool {
	switch category {
	case "", "all", "feed", "notes", "articles", "posts":
		return true
	case "photos":
		return socialHasMedia(row, "image")
	case "videos":
		return socialHasMedia(row, "video")
	case "podcasts":
		return socialHasMedia(row, "audio")
	default:
		return false
	}
}

func socialHasMedia(row event.Event, family string) bool {
	knownURLs := map[string]bool{}
	for _, tag := range row.Tags {
		if len(tag) < 2 {
			continue
		}
		if tag[0] == "image" {
			if socialHTTPURL(tag[1]) {
				knownURLs[strings.TrimSpace(tag[1])] = true
				if family == "image" {
					return true
				}
			}
			continue
		}
		if tag[0] != "imeta" {
			continue
		}
		var raw, mime string
		for _, field := range tag[1:] {
			if strings.HasPrefix(field, "m ") {
				mime = strings.TrimSpace(strings.TrimPrefix(field, "m "))
			}
			if strings.HasPrefix(field, "url ") {
				raw = strings.TrimSpace(strings.TrimPrefix(field, "url "))
			}
		}
		if socialHTTPURL(raw) {
			knownURLs[raw] = true
			if mediaFamily := socialMediaMIMEFamily(mime); mediaFamily != "" {
				if mediaFamily == family {
					return true
				}
			} else if socialMediaURLFamily(raw) == family {
				return true
			}
		}
	}
	content := socialContentOutsideFences(row.Content)
	if row.Kind == 30023 {
		for _, raw := range socialMarkdownImage.FindAllStringSubmatch(content, -1) {
			if len(raw) > 1 && socialHTTPURL(raw[1]) && !knownURLs[strings.TrimSpace(raw[1])] && family == "image" {
				return true
			}
		}
	}
	for _, raw := range socialContentURL.FindAllString(content, -1) {
		if !knownURLs[strings.TrimSpace(raw)] && socialMediaURLFamily(raw) == family {
			return true
		}
	}
	return false
}

func socialMediaMIMEFamily(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	switch {
	case strings.HasPrefix(mime, "image/") && mime != "image/svg+xml":
		return "image"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	default:
		return ""
	}
}

func socialMediaURLFamily(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), ".,;:!?")
	parsed, err := url.Parse(raw)
	if err != nil || !socialHTTPURL(raw) || parsed.Hostname() == "" {
		return ""
	}
	switch strings.ToLower(path.Ext(parsed.Path)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif", ".apng":
		return "image"
	case ".mp4", ".m4v", ".webm", ".mov":
		return "video"
	case ".mp3", ".m4a", ".wav", ".ogg", ".opus":
		return "audio"
	default:
		return ""
	}
}

func socialHTTPURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && parsed.User == nil && parsed.Hostname() != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func socialContentOutsideFences(content string) string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var out []string
	marker := ""
	for _, line := range lines {
		if marker != "" {
			if views.ClosesFence(line, marker) {
				marker = ""
			}
			continue
		}
		if next, _, ok := views.OpenFence(line); ok {
			marker = next
			continue
		}
		out = append(out, socialContentOutsideInlineCode(line))
	}
	return strings.Join(out, "\n")
}

func socialContentOutsideInlineCode(line string) string {
	var out strings.Builder
	for len(line) > 0 {
		start := strings.IndexByte(line, '`')
		if start < 0 {
			out.WriteString(line)
			break
		}
		out.WriteString(line[:start])
		run := 1
		for start+run < len(line) && line[start+run] == '`' {
			run++
		}
		marker := strings.Repeat("`", run)
		end := strings.Index(line[start+run:], marker)
		if end < 0 {
			out.WriteString(line[start:])
			break
		}
		line = line[start+run+end+run:]
	}
	return out.String()
}

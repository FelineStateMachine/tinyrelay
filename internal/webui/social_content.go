package webui

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"html"
	"html/template"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/views"
)

// socialBody renders user supplied social content with the same small
// Markdown grammar used by chat. Media is deliberately limited to ordinary
// HTTP(S) URLs and native browser elements.
func (a *App) socialBody(value any) template.HTML {
	return socialBodyWith(value, a.blocks())
}

func socialBody(value any) template.HTML {
	return socialBodyWith(value, nil)
}

func socialBodyWith(value any, blocks blockRenderer) template.HTML {
	content := plainString(value)
	var attachments []roomAttachment
	breaks := true
	if m := valueMap(value); m != nil {
		content = plainString(m["content"])
		attachments = socialAttachments(m)
		kind := plainString(m["kind"])
		breaks = kind != "30023"
	}
	nonce := rand.Text()
	tokenFor := func(raw string) string { return nonce + socialMediaToken(raw) }
	for _, item := range attachments {
		if !breaks {
			content = replaceSocialMedia(content, item.URL, tokenFor(item.URL))
		}
	}
	content = replaceStandaloneSocialMedia(content, attachments, tokenFor)
	var rendered string
	if breaks && valueMap(value) != nil {
		// NIP-01 notes and NIP-22 comments are plaintext. Preserve their
		// newlines and links without interpreting author text as Markdown.
		raw := html.EscapeString(content)
		raw = strings.ReplaceAll(raw, "\n", "<br>")
		rendered = autolinkOutsideTags(raw)
	} else if breaks {
		rendered = string(renderChatMarkdownWith(content, blocks))
	} else {
		rendered = string(renderMarkdownWith(content, blocks))
	}
	for _, item := range attachments {
		token := html.EscapeString(tokenFor(item.URL))
		rendered = strings.ReplaceAll(rendered, "<p>"+token+"</p>", socialMediaFigure(item))
		rendered = strings.ReplaceAll(rendered, token, socialMediaInline(item))
	}
	return template.HTML(socialNostrLinks(socialMediaDimensions(rendered, value)))
}

func socialMediaDimensions(rendered string, value any) string {
	for _, tag := range roomTags(value) {
		if len(tag) < 2 || tag[0] != "imeta" {
			continue
		}
		var raw, dimensions string
		for _, field := range tag[1:] {
			if strings.HasPrefix(field, "url ") {
				raw = socialMediaURL(strings.TrimPrefix(field, "url "))
			}
			if strings.HasPrefix(field, "dim ") {
				dimensions = strings.TrimPrefix(field, "dim ")
			}
		}
		widthText, heightText, ok := strings.Cut(dimensions, "x")
		if raw == "" || !ok {
			continue
		}
		width, widthErr := strconv.ParseFloat(widthText, 64)
		height, heightErr := strconv.ParseFloat(heightText, 64)
		if widthErr != nil || heightErr != nil || !(width >= 1 && width <= 32000 && height >= 1 && height <= 32000) {
			continue
		}
		prefix := `<img src="` + html.EscapeString(raw) + `" `
		attributes := `width="` + strconv.Itoa(int(width)) + `" height="` + strconv.Itoa(int(height)) + `" `
		rendered = strings.ReplaceAll(rendered, prefix, prefix+attributes)
	}
	return rendered
}

// socialPreview returns a compact plain-text excerpt suitable for feed cards.
func socialPreview(value any) string {
	text := plainString(value)
	if m := valueMap(value); m != nil {
		text = plainString(m["content"])
	}
	text = regexp.MustCompile("(?s)```.*?```|`([^`]+)`|!\\[([^\\]]*)\\]\\([^)]*\\)").ReplaceAllString(text, "$1$2")
	text = strings.Join(strings.Fields(text), " ")
	if len([]rune(text)) > 280 {
		text = string([]rune(text)[:277]) + "..."
	}
	return text
}

// socialMediaURL accepts only absolute HTTP(S) URLs without userinfo.
func socialMediaURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.String()
}

var socialImage = regexp.MustCompile(`!\[([^\]]*)\]\((https?://[^)\s]+)\)`)

func socialMediaToken(raw string) string {
	digest := sha256.Sum256([]byte(raw))
	return "social-media-" + hex.EncodeToString(digest[:])
}

func replaceSocialMedia(content, raw, replacement string) string {
	return replaceSocialOutsideCode(content, func(part string) string {
		return roomAttachmentReference.ReplaceAllStringFunc(part, func(match string) string {
			parts := roomAttachmentReference.FindStringSubmatch(match)
			if len(parts) == 4 && parts[2] == raw {
				return replacement
			}
			return match
		})
	})
}

func replaceSocialOutsideCode(content string, replace func(string) string) string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	marker := ""
	for i, line := range lines {
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
		lines[i] = replaceSocialInlineCode(line, replace)
	}
	return strings.Join(lines, "\n")
}

func replaceSocialInlineCode(line string, replace func(string) string) string {
	var out strings.Builder
	for len(line) > 0 {
		start := strings.IndexByte(line, '`')
		if start < 0 {
			out.WriteString(replace(line))
			break
		}
		out.WriteString(replace(line[:start]))
		run := 1
		for run < len(line)-start && line[start+run] == '`' {
			run++
		}
		end := strings.Index(line[start+run:], strings.Repeat("`", run))
		if end < 0 {
			out.WriteString(line[start:])
			break
		}
		end += start + run
		out.WriteString(line[start : end+run])
		line = line[end+run:]
	}
	return out.String()
}

func replaceStandaloneSocialMedia(content string, items []roomAttachment, tokenFor func(string) string) string {
	known := make(map[string]bool, len(items))
	for _, item := range items {
		known[item.URL] = true
	}
	return replaceSocialOutsideCode(content, func(line string) string {
		trimmed := strings.TrimSpace(line)
		if raw := socialMediaURL(strings.TrimRight(trimmed, ".,;:!?")); raw != "" && raw == trimmed && known[raw] {
			return tokenFor(raw)
		}
		return line
	})
}

func socialMediaFigure(item roomAttachment) string {
	raw := socialMediaURL(item.URL)
	if raw == "" {
		return ""
	}
	href := template.HTMLEscapeString(raw)
	alt := template.HTMLEscapeString(roomAttachmentLabel(item))
	if alt == "" {
		alt = "Media"
	}
	var media string
	mime := strings.ToLower(item.MIME)
	if strings.HasPrefix(mime, "image/") && mime != "image/svg+xml" {
		media = `<img src="` + href + `" alt="` + alt + `" loading="lazy" referrerpolicy="no-referrer">`
	} else if strings.HasPrefix(mime, "video/") {
		media = `<video controls preload="metadata" playsinline referrerpolicy="no-referrer" src="` + href + `"></video>`
	} else if strings.HasPrefix(mime, "audio/") {
		media = `<audio controls preload="metadata" src="` + href + `"></audio>`
	} else {
		return `<a data-social-media href="` + href + `" rel="noopener noreferrer" fx-ignore>` + html.EscapeString(path.Base(raw)) + `</a>`
	}
	return `<figure data-social-media>` + media + `</figure>`
}

func socialMediaInline(item roomAttachment) string {
	raw := socialMediaURL(item.URL)
	if raw == "" {
		return ""
	}
	href := template.HTMLEscapeString(raw)
	alt := template.HTMLEscapeString(roomAttachmentLabel(item))
	if alt == "" {
		alt = "Media"
	}
	switch mime := strings.ToLower(item.MIME); {
	case strings.HasPrefix(mime, "image/") && mime != "image/svg+xml":
		return `<img src="` + href + `" alt="` + alt + `" loading="lazy" referrerpolicy="no-referrer">`
	case strings.HasPrefix(mime, "video/"):
		return `<video controls preload="metadata" playsinline referrerpolicy="no-referrer" src="` + href + `"></video>`
	case strings.HasPrefix(mime, "audio/"):
		return `<audio controls preload="metadata" src="` + href + `"></audio>`
	default:
		return `<a data-social-media href="` + href + `" rel="noopener noreferrer" fx-ignore>` + html.EscapeString(path.Base(raw)) + `</a>`
	}
}

// Media metadata is preferred, while familiar file extensions keep older
// clients' ordinary image and video links useful without a server fetch.
func socialAttachments(row map[string]any) []roomAttachment {
	items := roomAttachments(row)
	seen := map[string]bool{}
	for i := range items {
		if !strings.Contains(items[i].MIME, "/") {
			items[i].MIME = socialMIME(items[i].URL, items[i].MIME)
		}
		seen[items[i].URL] = true
	}
	content := plainString(row["content"])
	replaceSocialOutsideCode(content, func(text string) string {
		for _, match := range roomAttachmentReference.FindAllStringSubmatch(text, -1) {
			raw := attachmentReferenceURL(match)
			if seen[raw] || socialMediaURL(raw) == "" || len(items) >= 16 {
				continue
			}
			typ := socialMIME(raw, "")
			if typ == "" && strings.HasPrefix(match[0], "![") && plainString(row["kind"]) == "30023" {
				typ = "image/jpeg"
			}
			if typ == "" {
				continue
			}
			seen[raw] = true
			items = append(items, roomAttachment{URL: raw, MIME: typ, Alt: match[1]})
		}
		return text
	})
	return items
}
func socialMIME(raw, short string) string {
	if short == "" {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		short = strings.TrimPrefix(strings.ToLower(path.Ext(u.Path)), ".")
	}
	return map[string]string{"jpg": "image/jpeg", "jpeg": "image/jpeg", "png": "image/png", "gif": "image/gif", "webp": "image/webp", "avif": "image/avif", "apng": "image/apng", "mp4": "video/mp4", "m4v": "video/mp4", "webm": "video/webm", "mov": "video/quicktime", "mp3": "audio/mpeg", "m4a": "audio/mp4", "wav": "audio/wav", "ogg": "audio/ogg", "opus": "audio/ogg"}[strings.ToLower(short)]
}

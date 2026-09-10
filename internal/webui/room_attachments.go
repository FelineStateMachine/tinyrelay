package webui

import (
	"encoding/json"
	"html/template"
	"net/url"
	"path"

	"regexp"
	"strconv"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/views"
)

const maxRoomAttachments = 16

type roomAttachment struct {
	URL, MIME, Hash, Name, Alt string
	Size                       int64
}

var roomAttachmentURL = regexp.MustCompile(`^(https?://[^\s\x00-\x1f\x7f\\<>"']+)$`)
var roomAttachmentReference = regexp.MustCompile(`!?\[((?:\\.|[^\]\\])*)\]\(([^)\s]+)\)|(https?://[^\s<>"']+)`)

func roomAttachments(value any) []roomAttachment {
	content := plainString(valueMap(value)["content"])
	items := roomAttachmentsFromTags(roomTags(value))
	out := items[:0]
	seen := map[string]bool{}
	for _, item := range items {
		if seen[item.URL] || !roomAttachmentURLVisible(content, item.URL) {
			continue
		}
		seen[item.URL] = true
		out = append(out, item)
	}
	return out
}

func roomAttachmentsFromTags(tags [][]string) []roomAttachment {
	var out []roomAttachment
	for _, tag := range tags {
		if len(tag) < 2 || tag[0] != "imeta" || len(out) >= maxRoomAttachments {
			continue
		}
		item := roomAttachment{}
		for _, field := range tag[1:] {
			key, val, ok := strings.Cut(field, " ")
			if !ok || val == "" {
				continue
			}
			switch key {
			case "url":
				item.URL = val
			case "m":
				item.MIME = strings.ToLower(val)
			case "x":
				item.Hash = val
			case "size":
				item.Size, _ = strconv.ParseInt(val, 10, 64)
			case "filename", "name":
				item.Name = val
			case "alt":
				item.Alt = val
			}
		}
		if !roomAttachmentURL.MatchString(item.URL) || !safeRoomAttachmentURL(item.URL) {
			continue
		}
		if item.Size < 0 || item.Size > 1<<40 {
			item.Size = 0
		}
		item.Name = boundedAttachmentText(item.Name, 255)
		item.Alt = boundedAttachmentText(item.Alt, 500)
		if item.Name == "" {
			item.Name = item.Alt
		}
		if item.Name == "" {
			u, _ := url.Parse(item.URL)
			item.Name = path.Base(u.Path)
			if item.Name == "." || item.Name == "/" {
				item.Name = "Attachment"
			}
		}
		out = append(out, item)
	}
	return out
}

func roomTags(value any) [][]string {
	var tags [][]string
	encoded, err := json.Marshal(valueMap(value)["tags"])
	if err == nil {
		_ = json.Unmarshal(encoded, &tags)
	}
	return tags
}

func safeRoomAttachmentURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.User == nil && u.Hostname() != "" && (u.Scheme == "http" || u.Scheme == "https")
}

// attachmentLines visits only lines outside fenced code, retaining original
// lines so removing a standalone reference cannot rewrite prose or examples.
func attachmentLines(content string, visit func(string) string) string {
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
		lines[i] = visit(line)
	}
	return strings.Join(lines, "\n")
}

func attachmentOutsideCode(line string) string {
	var out strings.Builder
	for len(line) > 0 {
		start := strings.IndexByte(line, '`')
		if start < 0 {
			out.WriteString(line)
			break
		}
		out.WriteString(line[:start])
		line = line[start:]
		n := len(line) - len(strings.TrimLeft(line, "`"))
		end := strings.Index(line[n:], line[:n])
		if end < 0 {
			out.WriteString(line)
			break
		}
		out.WriteByte(' ')
		line = line[n+end+n:]
	}
	return out.String()
}

func attachmentReferenceURL(match []string) string {
	if match[2] != "" {
		return match[2]
	}
	return strings.TrimRight(match[3], ".,;:!?)")
}

func roomAttachmentContent(content string, items []roomAttachment) string {
	urls := make(map[string]bool, len(items))
	for _, item := range items {
		urls[item.URL] = true
	}
	return strings.TrimSpace(attachmentLines(content, func(line string) string {
		trimmed := strings.TrimSpace(line)
		match := roomAttachmentReference.FindStringSubmatch(trimmed)
		if match != nil && match[0] == trimmed && urls[attachmentReferenceURL(match)] {
			return ""
		}
		return line
	}))
}

func roomAttachmentURLVisible(content, target string) bool {
	visible := false
	attachmentLines(content, func(line string) string {
		for _, match := range roomAttachmentReference.FindAllStringSubmatch(attachmentOutsideCode(line), -1) {
			if attachmentReferenceURL(match) == target {
				visible = true
			}
		}
		return line
	})
	return visible
}

func boundedAttachmentText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

func roomAttachmentLabel(item roomAttachment) string {
	if item.Alt != "" {
		return item.Alt
	}
	return item.Name
}

func roomAttachmentMarkup(items []roomAttachment) template.HTML {
	if len(items) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString(`<div data-room-attachments>`)
	for _, item := range items {
		out.WriteString(roomAttachmentFigure(item))
	}
	out.WriteString(`</div>`)
	return template.HTML(out.String())
}

func roomAttachmentFigure(item roomAttachment) string {
	href := template.HTMLEscapeString(item.URL)
	name := template.HTMLEscapeString(item.Name)
	if name == "" {
		name = "Attachment"
	}
	alt := template.HTMLEscapeString(roomAttachmentLabel(item))
	var out strings.Builder
	out.WriteString(`<figure>`)
	switch {
	case strings.HasPrefix(item.MIME, "image/") && item.MIME != "image/svg+xml":
		out.WriteString(`<a href="` + href + `" rel="noopener noreferrer" fx-ignore><img src="` + href + `" alt="` + alt + `" loading="lazy" referrerpolicy="no-referrer"></a>`)
	case strings.HasPrefix(item.MIME, "video/") || strings.HasPrefix(item.MIME, "audio/"):
		kind := "audio"
		if strings.HasPrefix(item.MIME, "video/") {
			kind = "video"
		}
		out.WriteString(`<` + kind + ` controls preload="none" src="` + href + `"></` + kind + `>`)
	}
	out.WriteString(`<figcaption><a data-download href="` + href + `" download="` + name + `" rel="noopener noreferrer" fx-ignore>` + name + `</a>`)
	if item.Size > 0 {
		out.WriteString(` <small>` + roomFileSize(item.Size) + `</small>`)
	}
	out.WriteString(`</figcaption></figure>`)
	return out.String()
}

func roomFileSize(size int64) string {
	if size < 1024 {
		return strconv.FormatInt(size, 10) + " B"
	}
	if size < 1<<20 {
		return strconv.FormatInt(size/1024, 10) + " KB"
	}
	return strconv.FormatInt(size/(1<<20), 10) + " MB"
}

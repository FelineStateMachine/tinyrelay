package daemon

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/mcp"
)

var mcpAttachment = mcp.Object(map[string]any{
	"url":      mcpText,
	"sha256":   mcpHash,
	"size":     map[string]any{"type": "integer", "minimum": 0},
	"type":     mcpText,
	"filename": mcpText,
	"room":     mcpRoomID,
	"uploaded": mcpUnixTime,
	"nip94":    map[string]any{"type": "array"},
}, "url", "sha256", "size", "type")

func mcpAttachmentURL(raw string) error {
	if strings.ContainsAny(raw, "\\<>\"'()") || strings.IndexFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return errors.New("attachment URL contains unsafe characters; percent-encode them")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return errors.New("attachment URL must be absolute HTTP or HTTPS without credentials")
	}
	return nil
}

func mcpAttachmentMIME(raw string) error {
	typ, _, err := mime.ParseMediaType(raw)
	if err != nil || !strings.Contains(typ, "/") || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return errors.New("attachment type must be a MIME type")
	}
	return nil
}

func mcpAttachmentTags(call mcp.Call) (string, [][]string, error) {
	values, _ := call.Arguments["attachments"].([]any)
	if len(values) > 8 {
		return "", nil, errors.New("attach up to 8 files per message")
	}
	var links []string
	var tags [][]string
	for _, raw := range values {
		item, ok := raw.(map[string]any)
		if !ok {
			return "", nil, errors.New("each attachment must be an object")
		}
		link, _ := item["url"].(string)
		hash, _ := item["sha256"].(string)
		typ, _ := item["type"].(string)
		size, ok := item["size"].(float64)
		if !ok || size < 0 || size != float64(int64(size)) || !hex64(hash) {
			return "", nil, errors.New("attachment needs url, sha256, non-negative integer size and type")
		}
		if err := mcpAttachmentURL(link); err != nil {
			return "", nil, err
		}
		if err := mcpAttachmentMIME(typ); err != nil {
			return "", nil, err
		}
		if room, _ := item["room"].(string); room != "" && room != call.String("room") {
			return "", nil, errors.New("upload the attachment to the destination room first")
		}
		filename, _ := item["filename"].(string)
		filename = roomAttachmentFilename(filename)
		tags = append(tags, []string{"imeta", "url " + link, "m " + typ, "x " + hash, "size " + strconv.FormatInt(int64(size), 10), "filename " + filename})
		mediaType, _, _ := mime.ParseMediaType(typ)
		switch {
		case strings.HasPrefix(mediaType, "image/"):
			links = append(links, "![image]("+link+")")
		case strings.HasPrefix(mediaType, "video/"):
			links = append(links, "![video]("+link+")")
		default:
			label := strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]").Replace(filename)
			links = append(links, "["+label+"]("+link+")")
		}
	}
	return strings.Join(links, "\n"), tags, nil
}

func mcpRoomContent(call mcp.Call) (string, [][]string, error) {
	content, tags, err := mcpAttachmentTags(call)
	if err != nil {
		return "", nil, err
	}
	if text := call.String("content"); strings.TrimSpace(text) != "" {
		if content != "" {
			content = text + "\n" + content
		} else {
			content = text
		}
	}
	return content, tags, nil
}

func mcpCheckAttachments(e event.Event) error {
	for _, tag := range e.Tags {
		if len(tag) == 0 || tag[0] != "imeta" {
			continue
		}
		values := map[string]string{}
		for _, field := range tag[1:] {
			parts := strings.SplitN(field, " ", 2)
			if len(parts) == 2 {
				values[parts[0]] = parts[1]
			}
		}
		if values["url"] == "" || !hex64(values["x"]) || strings.TrimSpace(values["m"]) == "" {
			return errors.New("imeta attachment needs url, m and x fields")
		}
		if err := mcpAttachmentURL(values["url"]); err != nil {
			return err
		}
		if err := mcpAttachmentMIME(values["m"]); err != nil {
			return err
		}
		if size := values["size"]; size != "" {
			n, err := strconv.ParseInt(size, 10, 64)
			if err != nil || n < 0 {
				return errors.New("imeta size must be a non-negative integer")
			}
		}
	}
	return nil
}

func (t *Tenant) mcpUploadAttachment(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	if !t.Policy().Features.Files || t.blobs == nil {
		return mcp.Failure("file storage is disabled", nil), nil
	}
	room := call.String("room")
	if len(call.String("data")) > 700*1024 {
		return mcp.Failure("Base64 data exceeds 700 KiB; use the HTTP room attachment endpoint for larger files", nil), nil
	}
	raw, err := base64.StdEncoding.DecodeString(call.String("data"))
	if err != nil {
		return mcp.Failure("data must be standard Base64", nil), nil
	}
	if int64(len(raw)) > roomAttachmentMaxBytes {
		return mcp.Failure("attachment exceeds the 32 MiB room limit", nil), nil
	}
	typ := strings.TrimSpace(call.String("type"))
	if typ == "" || strings.ContainsAny(typ, "\r\n") {
		return mcp.Failure("type is required", nil), nil
	}
	descriptor, err := t.storeRoomAttachment(ctx, call.Actor, room, roomAttachmentInput{Body: raw, Type: typ, Filename: call.String("filename")})
	if err != nil {
		return mcp.Failure("upload failed: "+err.Error(), nil), nil
	}
	return mcp.Value(descriptor), nil
}

func (t *Tenant) mcpReadAttachment(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	if !t.Policy().Features.Files || t.blobs == nil {
		return mcp.Failure("file storage is disabled", nil), nil
	}
	if err := t.browseRead(ctx, call.Actor); err != nil {
		return mcp.Failure("attachment unavailable: "+err.Error(), nil), nil
	}
	if err := t.roomAttachmentAccess(ctx, call.Actor, call.String("sha256")); err != nil {
		return mcp.Failure("attachment unavailable: "+err.Error(), nil), nil
	}
	entry, body, err := t.blobs.Get(ctx, call.String("sha256"))
	if err != nil {
		return mcp.Failure("attachment unavailable: "+err.Error(), nil), nil
	}
	defer body.Close()
	max := call.Int("max_bytes")
	if max == 0 {
		max = 4 * 1024 * 1024
	}
	if max < 1 || max > 4*1024*1024 {
		return mcp.Failure("max_bytes must be between 1 and 4194304", nil), nil
	}
	if entry.Size > int64(max) {
		return mcp.Failure("attachment exceeds max_bytes", map[string]any{"size": entry.Size}), nil
	}
	raw, err := io.ReadAll(io.LimitReader(body, int64(max)+1))
	if err != nil {
		return mcp.Result{}, err
	}
	if len(raw) > max {
		return mcp.Failure("attachment exceeds max_bytes", nil), nil
	}
	descriptor := t.blobs.Descriptor(strings.TrimRight(t.publicURL, "/"), entry)
	descriptor["data"] = base64.StdEncoding.EncodeToString(raw)
	if strings.HasPrefix(entry.Type, "text/") {
		descriptor["text"] = string(raw)
	}
	result := mcp.Result{StructuredContent: descriptor}
	mediaType := ""
	if strings.HasPrefix(entry.Type, "image/") {
		mediaType = "image"
	}
	if strings.HasPrefix(entry.Type, "audio/") {
		mediaType = "audio"
	}
	if mediaType != "" {
		result.Content = []mcp.Content{{Type: mediaType, Data: base64.StdEncoding.EncodeToString(raw), MimeType: entry.Type}}
	} else if strings.HasPrefix(entry.Type, "text/") {
		result.Content = []mcp.Content{{Type: "text", Text: string(raw)}}
	} else {
		result.Content = []mcp.Content{{Type: "text", Text: "File bytes are in structuredContent.data as Base64."}}
	}
	return result, nil
}

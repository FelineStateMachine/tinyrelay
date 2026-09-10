package webui

import (
	"strings"
	"testing"
)

func TestRoomAttachmentRenderingIntegration(t *testing.T) {
	url := "https://cdn.example.test/report.pdf"
	row := map[string]any{"content": "See [report](" + url + ") and keep this prose.", "tags": [][]string{{"imeta", "url " + url, "m application/pdf", "filename report.pdf"}}}
	items := roomAttachments(row)
	if len(items) != 1 {
		t.Fatalf("items = %#v", items)
	}
	if got := roomAttachmentContent(row["content"].(string), items); got != row["content"].(string) {
		t.Fatalf("generic markdown changed: %q", got)
	}
	markup := string(roomAttachmentMarkup(items))
	if strings.Contains(markup, "[report]()") || !strings.Contains(markup, "report.pdf") {
		t.Fatalf("generic attachment markup: %s", markup)
	}
}

func TestRoomAttachmentVisibleURLMatchingIsExactAndSafe(t *testing.T) {
	url := "https://cdn.example.test/photo.png"
	base := [][]string{{"imeta", "url " + url, "m image/png"}}
	for name, content := range map[string]string{
		"prefix": url + "-backup",
		"fenced": "```\n" + url + "\n```",
		"inline": "`" + url + "`",
	} {
		if got := roomAttachments(map[string]any{"content": content, "tags": base}); len(got) != 0 {
			t.Errorf("%s matched %#v", name, got)
		}
	}
	for _, unsafe := range []string{"javascript:alert(1)", "data:image/png,abc", "//cdn.example/photo.png", "/media/" + strings.Repeat("a", 64)} {
		if got := roomAttachments(map[string]any{"content": unsafe, "tags": [][]string{{"imeta", "url " + unsafe, "m image/png"}}}); len(got) != 0 {
			t.Errorf("unsafe URL %q accepted", unsafe)
		}
	}
}

func TestRoomAttachmentEditAndThreadRootReplacement(t *testing.T) {
	rootID := strings.Repeat("1", 64)
	oldURL := "https://cdn.example.test/old.png"
	newURL := "https://cdn.example.test/new.mp4"
	root := map[string]any{"id": rootID, "pubkey": strings.Repeat("a", 64), "kind": 11, "created_at": int64(10), "content": oldURL, "tags": [][]string{{"imeta", "url " + oldURL, "m image/png"}}}
	edit := map[string]any{"id": strings.Repeat("2", 64), "pubkey": root["pubkey"], "kind": 40003, "created_at": int64(11), "content": newURL, "tags": [][]string{{"e", rootID}, {"imeta", "url " + newURL, "m video/mp4"}}}
	value := map[string]any{"room": map[string]any{"id": "general"}, "root": root, "replies": []any{edit}}
	item := roomRoot(value)
	if !item.Edited || item.Content != newURL || len(item.Attachments) != 1 || item.Attachments[0].URL != newURL {
		t.Fatalf("root edit = %#v", item)
	}
}

func TestRoomAttachmentMediaMarkupHasFallbackAndBounds(t *testing.T) {
	items := []roomAttachment{{URL: "https://cdn.example.test/a.mp4", MIME: "video/mp4", Name: "clip.mp4"}, {URL: "https://cdn.example.test/a.mp3", MIME: "audio/mpeg", Name: "audio.mp3"}}
	markup := string(roomAttachmentMarkup(items))
	if !strings.Contains(markup, "clip.mp4") || !strings.Contains(markup, "audio.mp3") {
		t.Fatalf("media fallback missing: %s", markup)
	}
}

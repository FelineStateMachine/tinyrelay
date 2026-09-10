package webui

import (
	"strings"
	"testing"
)

func TestRoomAttachmentsParsesNIP92AndStripsReferences(t *testing.T) {
	url := "https://cdn.example.test/photo.png"
	row := map[string]any{"content": "Look ![photo](" + url + ") and " + url, "tags": [][]string{{"imeta", "url " + url, "m image/png", "x " + strings.Repeat("a", 64), "size 42", "alt a <photo>"}}}
	got := roomAttachments(row)
	if len(got) != 1 || got[0].URL != url || got[0].MIME != "image/png" || got[0].Hash != strings.Repeat("a", 64) || got[0].Size != 42 || got[0].Name != "a <photo>" {
		t.Fatalf("attachments = %#v", got)
	}
	if text := roomAttachmentContent(valueMap(row)["content"].(string), got); text != valueMap(row)["content"].(string) {
		t.Fatalf("content = %q", text)
	}
}

func TestRoomAttachmentsRejectUnsafeAndBoundValues(t *testing.T) {
	long := strings.Repeat("x", 5000)
	row := map[string]any{"content": "https://cdn.example/file", "tags": [][]string{{"imeta", "url javascript:alert(1)", "m image/svg+xml"}, {"imeta", "url data:image/png;base64,abc", "m image/png"}, {"imeta", "url https://cdn.example/file", "m application/octet-stream", "filename " + long}}}
	got := roomAttachments(row)
	if len(got) != 1 || got[0].URL != "https://cdn.example/file" {
		t.Fatalf("unsafe attachments = %#v", got)
	}
}

func TestRoomAttachmentsIgnoreCodeAndDeduplicate(t *testing.T) {
	url := "https://cdn.example.test/photo.png"
	row := map[string]any{"content": "`" + url + "`\n" + url, "tags": [][]string{{"imeta", "url " + url, "m image/png"}, {"imeta", "url " + url, "m image/png"}}}
	if got := roomAttachments(row); len(got) != 1 {
		t.Fatalf("attachments = %#v", got)
	}
	row["content"] = "```\n" + url + "\n```"
	if got := roomAttachments(row); len(got) != 0 {
		t.Fatalf("code attachment = %#v", got)
	}
}

func TestRoomAttachmentMarkupEscapesAndClassifiesMedia(t *testing.T) {
	items := []roomAttachment{{URL: "https://cdn.example/a.mp4?a=1&b=2", MIME: "video/mp4", Name: "A \"clip\""}, {URL: "https://cdn.example/report.pdf", MIME: "application/pdf", Name: "report.pdf", Size: 2048}}
	markup := string(roomAttachmentMarkup(items))
	for _, want := range []string{"room-attachments", "<video", "controls", "report.pdf", "download", "2 KB"} {
		if !strings.Contains(markup, want) {
			t.Errorf("markup missing %q: %s", want, markup)
		}
	}
	if strings.Contains(markup, `class=`) || strings.Contains(markup, `javascript:`) {
		t.Fatalf("unsafe markup: %s", markup)
	}
}

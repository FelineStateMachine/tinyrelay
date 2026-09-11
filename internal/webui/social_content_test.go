package webui

import (
	"strings"
	"testing"
)

func TestSocialBodyRendersMarkdownAndMedia(t *testing.T) {
	row := map[string]any{
		"kind":    "30023",
		"content": "# Hello\n\nA **note** with a table:\n\n| one | two |\n| --- | --- |\n| a | b |\n\n![sun](https://cdn.example.test/sun.png)",
		"tags":    [][]string{{"imeta", "url https://cdn.example.test/sun.png", "m image/png", "alt sunset"}},
	}
	got := string(socialBody(row))
	for _, want := range []string{"<h1>Hello</h1>", "<strong>note</strong>", "data-markdown-table", "<img", `alt="sunset"`, `loading="lazy"`} {
		if !strings.Contains(got, want) {
			t.Errorf("social body missing %q: %s", want, got)
		}
	}
	if strings.Count(got, "sun.png") != 1 {
		t.Fatalf("media should have one image: %s", got)
	}
}

func TestSocialBodySupportsMediaTypesAndIgnoresCode(t *testing.T) {
	row := map[string]any{
		"kind":    "1",
		"content": "https://cdn.example.test/clip.mp4\n\nhttps://cdn.example.test/song.mp3\n\n```\nhttps://cdn.example.test/hidden.png\n```",
		"tags":    [][]string{{"imeta", "url https://cdn.example.test/clip.mp4", "m video/mp4"}, {"imeta", "url https://cdn.example.test/song.mp3", "m audio/mpeg"}, {"imeta", "url https://cdn.example.test/hidden.png", "m image/png"}},
	}
	got := string(socialBody(row))
	if !strings.Contains(got, `<video controls`) || !strings.Contains(got, `<audio controls`) {
		t.Fatalf("native media missing: %s", got)
	}
	if !strings.Contains(got, "hidden.png") || strings.Contains(got, "<iframe") {
		t.Fatalf("code or iframe leaked into media: %s", got)
	}
}

func TestSocialNoteIsPlaintext(t *testing.T) {
	got := string(socialBody(map[string]any{"kind": "1", "content": "**literal**\nhttps://cdn.example.test/note.png", "tags": [][]string{{"imeta", "url https://cdn.example.test/note.png", "m image/png"}}}))
	if strings.Contains(got, "<strong>") || !strings.Contains(got, "**literal**") || !strings.Contains(got, "<img") {
		t.Fatalf("note rendering = %s", got)
	}
}

func TestSocialMediaSkipsInlineAndFencedCode(t *testing.T) {
	url := "https://cdn.example.test/hidden.png"
	row := map[string]any{"kind": "30023", "content": "`![x](" + url + ")`\n\n````\n![x](" + url + ")\n````", "tags": [][]string{{"imeta", "url " + url, "m image/png"}}}
	got := string(socialBody(row))
	if strings.Contains(got, "<img") || strings.Contains(got, "<figure") {
		t.Fatalf("code media was embedded: %s", got)
	}
	if !strings.Contains(got, "hidden.png") {
		t.Fatalf("code source missing: %s", got)
	}
}

func TestSocialBodyEscapesUnsafeContent(t *testing.T) {
	got := string(socialBody(map[string]any{"content": `<script>alert(1)</script> [x](javascript:alert(1)) https://cdn.example.test/a.svg`, "tags": [][]string{{"imeta", "url https://cdn.example.test/a.svg", "m image/svg+xml"}}}))
	if strings.Contains(got, "<script") || strings.Contains(got, `href="javascript:`) || strings.Contains(got, "<iframe") {
		t.Fatalf("unsafe social markup: %s", got)
	}
	if !strings.Contains(got, "a.svg") {
		t.Fatalf("safe media link missing: %s", got)
	}
}

func TestSocialPreviewAndMediaURL(t *testing.T) {
	if got := socialPreview(map[string]any{"content": "hello\nworld"}); got != "hello world" {
		t.Fatalf("preview = %q", got)
	}
	if got := socialMediaURL("javascript:alert(1)"); got != "" {
		t.Fatalf("unsafe URL = %q", got)
	}
	if got := socialMediaURL("https://example.test/a?x=1"); got != "https://example.test/a?x=1" {
		t.Fatalf("safe URL = %q", got)
	}
}

func TestSocialArticleMediaWithoutMetadata(t *testing.T) {
	got := string(socialBody(map[string]any{"kind": 30023, "content": "![Cover](https://cdn.test/a.jpg)\n\n[Watch](https://cdn.test/a.mp4)\n\nhttps://cdn.test/a.mp3"}))
	for _, want := range []string{`<img src="https://cdn.test/a.jpg"`, `alt="Cover"`, `<video controls`, `<audio controls`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s: %s", want, got)
		}
	}
	if strings.Contains(got, "!Cover") || strings.Contains(got, "<p><figure") {
		t.Fatal(got)
	}
}
func TestSocialNoteImageWithoutMetadataAndInvalidClientMIME(t *testing.T) {
	for _, tags := range [][][]string{nil, {{"imeta", "url https://cdn.test/a.jpg", "m jpeg"}}} {
		got := string(socialBody(map[string]any{"kind": 1, "content": "A photo\nhttps://cdn.test/a.jpg", "tags": tags}))
		if !strings.Contains(got, `<img src="https://cdn.test/a.jpg"`) {
			t.Fatal(got)
		}
	}
}
func TestSocialMediaTokenCannotBeForgedInCode(t *testing.T) {
	raw := "https://cdn.test/a.png"
	got := string(socialBody(map[string]any{"kind": 30023, "content": "`" + socialMediaToken(raw) + "`\n\n![A](" + raw + ")"}))
	if strings.Count(got, "<img") != 1 || !strings.Contains(got, "<code>"+socialMediaToken(raw)+"</code>") {
		t.Fatal(got)
	}
}

func TestSocialMediaReservesValidMetadataDimensions(t *testing.T) {
	for _, dimension := range []string{"1170x1613", "1170.0x1613.0"} {
		got := string(socialBody(map[string]any{"kind": 1, "content": "https://cdn.test/a.jpg", "tags": [][]string{{"imeta", "url https://cdn.test/a.jpg", "m image/jpeg", "dim " + dimension}}}))
		if !strings.Contains(got, `width="1170" height="1613"`) {
			t.Fatal(got)
		}
	}
	for _, dimension := range []string{"0x3", "NaNx3", "1e90x3", `1" onload="bad`} {
		got := string(socialBody(map[string]any{"kind": 1, "content": "https://cdn.test/a.jpg", "tags": [][]string{{"imeta", "url https://cdn.test/a.jpg", "m image/jpeg", "dim " + dimension}}}))
		if strings.Contains(got, "width=") || strings.Contains(got, "onload=") {
			t.Fatal(got)
		}
	}
}

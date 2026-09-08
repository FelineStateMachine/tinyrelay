package webui

import (
	"strings"
	"testing"
)

func TestInjectBasePreservesExternalURLsAndContent(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"local link", `<a href="/repo?path=a%26b">code</a>`, `<a href="/r/alice/repo?path=a%26b">code</a>`},
		{"external link", `<a href="//example.org/file">external</a>`, `<a href="//example.org/file">external</a>`},
		{"external image", `<img src="//example.org/icon.png">`, `<img src="//example.org/icon.png">`},
		{"already scoped", `<a href="/r/alice/files">files</a>`, `<a href="/r/alice/files">files</a>`},
		{"script source", `<script src="/fixi.js"></script>`, `<script src="/r/alice/fixi.js"></script>`},
		{"script text", `<script>const example = ' href="/example"';</script>`, `<script>const example = ' href="/example"';</script>`},
		{"textarea", `<textarea> href="/example"</textarea>`, `<textarea> href="/example"</textarea>`},
		{"local action", `<button fx-action="/connect/fragment">preview</button>`, `<button fx-action="/r/alice/connect/fragment">preview</button>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := injectBase(tc.input, "/r/alice"); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func FuzzInjectBasePreservesText(f *testing.F) {
	for _, seed := range []string{"hello", ` href="/file"`, `fetch("/example")`, `new URL("/file")`, "emoji 🧪", "a\x00b"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, content string) {
		// Textarea content can contain quotes and URL examples literally.
		content = strings.ReplaceAll(content, "<", "&lt;")
		input := "<textarea>" + content + "</textarea>"
		if got := injectBase(input, "/r/alice"); got != input {
			t.Fatalf("content changed: got %q, want %q", got, input)
		}
	})
}

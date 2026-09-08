package webui

import (
	"strings"
	"testing"
)

func TestMarkdownRendersCommonFormsAndEscapesTheRest(t *testing.T) {
	got := string(renderMarkdown("# Title\n\nA `code` span with **bold** and *em* and [a link](https://example.com).\n\n```sh\ngo build <x>\n```\n\n- one\n- two\n\n1. first\n2. second\n\n<script>alert(1)</script> and [bad](javascript:alert(1))"))
	for _, want := range []string{"<h1>Title</h1>", "<code>code</code>", "<strong>bold</strong>", "<em>em</em>", `<a href="https://example.com">a link</a>`, "<pre><code>go build &lt;x&gt;</code></pre>", "<ul><li>one</li><li>two</li></ul>", "<ol><li>first</li><li>second</li></ol>", "&lt;script&gt;alert(1)&lt;/script&gt;"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "<script>") || strings.Contains(got, `href="javascript:`) {
		t.Fatalf("unsafe markup leaked: %s", got)
	}
}

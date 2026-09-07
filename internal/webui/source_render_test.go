package webui

import (
	"html/template"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestSourceHTMLIsEscapedAndAnchored(t *testing.T) {
	got := sourceHTML(map[string]any{
		"path":    "internal/main.go",
		"content": "package main\nvar x = \"<script>alert(1)</script>\"\n",
	})
	html := string(got)
	for _, want := range []string{
		`class="source-view source-lang-go"`,
		`id="L1"`,
		`href="#L2"`,
		`class="tok-keyword">var</span>`,
		`&lt;script&gt;`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("source output missing %q: %s", want, html)
		}
	}
	if strings.Contains(html, "<script>alert") {
		t.Fatalf("source content was not escaped: %s", html)
	}
	if _, ok := any(got).(template.HTML); !ok {
		t.Fatalf("sourceHTML type = %T, want template.HTML", got)
	}
}

func TestSourceHTMLUsesPlaintextAndBoundsPreview(t *testing.T) {
	content := strings.Repeat("<unsafe>\n", sourcePreviewLimit/len("<unsafe>\n")+10)
	got := string(sourceHTML(map[string]any{"name": "README.unknown", "content": content}))
	if !strings.Contains(got, `class="source-view source-lang-text"`) {
		t.Fatalf("unknown extension should use plaintext: %s", got[:min(len(got), 200)])
	}
	if !strings.Contains(got, `class="source-truncated"`) {
		t.Fatal("bounded preview should identify truncation")
	}
	if strings.Contains(got, "<unsafe>") {
		t.Fatal("plaintext source was not escaped")
	}
}

func TestDiffHTMLClassifiesLinesWithoutHTMLInjection(t *testing.T) {
	got := string(diffHTML(map[string]any{"diff": "@@ -1 +1 @@\n-old\n+<script>bad</script>\n context"}))
	for _, want := range []string{`class="diff-hunk"`, `class="diff-remove"`, `class="diff-add"`, `class="diff-context"`, `&lt;script&gt;`} {
		if !strings.Contains(got, want) {
			t.Fatalf("diff output missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "<script>bad") {
		t.Fatal("diff content was not escaped")
	}
}

func TestPreviewBoundsLineMarkupAndDisclosesOmission(t *testing.T) {
	for name, render := range map[string]func(any) template.HTML{"source": sourceHTML, "diff": diffHTML} {
		t.Run(name, func(t *testing.T) {
			content := strings.Repeat("\n", sourcePreviewLimit)
			got := string(render(map[string]any{"content": content, "diff": content}))
			if len(got) > 2*1024*1024 {
				t.Fatalf("a short-line preview expanded to %d bytes", len(got))
			}
			if !strings.Contains(got, "10,000 lines") {
				t.Fatal("line preview omission was not disclosed")
			}
		})
	}
}

func TestDiffDisclosesBackendTruncation(t *testing.T) {
	got := string(diffHTML(map[string]any{"diff": "already bounded", "truncated": true}))
	if !strings.Contains(got, "truncated") {
		t.Fatal("backend truncation was hidden")
	}
}

func TestSourceDoesNotInventAnEmptyLineAtEndOfFile(t *testing.T) {
	got := string(sourceHTML(map[string]any{"content": "first\nsecond\n"}))
	if strings.Contains(got, `id="L3"`) || !strings.Contains(got, `id="L2"`) {
		t.Fatal("terminal newline added a phantom source row")
	}
}

func TestSourceHTMLPreservesRenderedSourceText(t *testing.T) {
	want := "/* block <tag> */\nfunc π(😀 string) {\n\treturn \"<script>\"\n}\n"
	root, err := html.Parse(strings.NewReader(string(sourceHTML(map[string]any{
		"path":    "unicode.go",
		"content": want,
	}))))
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "td" && hasClass(node, "source-code") {
			appendText(&got, node)
			got.WriteByte('\n')
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(root)
	if got.String() != want {
		t.Fatalf("rendered source changed content:\nwant %q\ngot  %q", want, got.String())
	}
}

func hasClass(node *html.Node, class string) bool {
	for _, attr := range node.Attr {
		if attr.Key == "class" && attr.Val == class {
			return true
		}
	}
	return false
}

func appendText(out *strings.Builder, node *html.Node) {
	if node.Type == html.TextNode {
		out.WriteString(node.Data)
		return
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		appendText(out, child)
	}
}

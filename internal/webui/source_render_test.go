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
		`<source-view lang="go"><table>`,
		`id="L1"`,
		`href="#L2"`,
		`<b>var</b>`,
		`<q>&#34;&lt;script&gt;alert(1)&lt;/script&gt;&#34;</q>`,
		`</table></source-view>`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("source output missing %q: %s", want, html)
		}
	}
	if strings.Contains(html, "<script>alert") || strings.Contains(html, "class=") {
		t.Fatalf("source content was not escaped or used class hooks: %s", html)
	}
	if _, ok := any(got).(template.HTML); !ok {
		t.Fatalf("sourceHTML type = %T, want template.HTML", got)
	}
}

func TestSourceHTMLUsesPlaintextAndBoundsPreview(t *testing.T) {
	content := strings.Repeat("<unsafe>\n", sourcePreviewLimit/len("<unsafe>\n")+10)
	got := string(sourceHTML(map[string]any{"name": "README.unknown", "content": content}))
	if !strings.Contains(got, `<source-view lang="text">`) {
		t.Fatalf("unknown extension should use plaintext: %s", got[:min(len(got), 200)])
	}
	if !strings.Contains(got, `<tfoot><tr><td colspan="2">Preview truncated`) {
		t.Fatal("bounded preview should identify truncation")
	}
	if strings.Contains(got, "<unsafe>") {
		t.Fatal("plaintext source was not escaped")
	}
}

func TestDiffHTMLClassifiesLinesWithoutHTMLInjection(t *testing.T) {
	got := string(diffHTML(map[string]any{"diff": "@@ -1 +1 @@\n-old\n+<script>bad</script>\n context"}))
	for _, want := range []string{`<diff-view><table>`, `<code><b>@@ -1 +1 @@</b></code>`, `<code><del>-old</del></code>`, `<code><ins>+&lt;script&gt;bad&lt;/script&gt;</ins></code>`, `<code> context</code>`, `</table></diff-view>`} {
		if !strings.Contains(got, want) {
			t.Fatalf("diff output missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "<script>bad") || strings.Contains(got, "class=") {
		t.Fatal("diff content was not escaped or used class hooks")
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
			if !strings.Contains(got, "<tfoot>") || !strings.Contains(got, "10,000 lines") {
				t.Fatal("line preview omission was not disclosed")
			}
		})
	}
}

func TestDiffDisclosesBackendTruncation(t *testing.T) {
	got := string(diffHTML(map[string]any{"diff": "already bounded", "truncated": true}))
	if !strings.Contains(got, "<tfoot>") || !strings.Contains(got, "truncated") {
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
		if node.Type == html.ElementNode && node.Data == "td" && isCodeCell(node) {
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

// isCodeCell reports whether a table cell holds rendered source rather than
// a line number anchor.
func isCodeCell(node *html.Node) bool {
	first := node.FirstChild
	return first != nil && first.Type == html.ElementNode && first.Data == "code"
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

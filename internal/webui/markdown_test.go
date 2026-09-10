package webui

import (
	"net/url"
	"strings"
	"testing"
)

func TestMarkdownRendersCommonFormsAndEscapesTheRest(t *testing.T) {
	got := string(renderMarkdown("# Title\n\nA `code` span with **bold** and *em* and [a link](https://example.com).\n\n```sh\ngo build <x>\n```\n\n- one\n- two\n\n1. first\n2. second\n\n<script>alert(1)</script> and [bad](javascript:alert(1))"))
	for _, want := range []string{"<h1>Title</h1>", "<code>code</code>", "<strong>bold</strong>", "<em>em</em>", `<a href="https://example.com">a link</a>`, `<pre><code data-lang="sh">go build &lt;x&gt;</code></pre>`, "<ul><li>one</li><li>two</li></ul>", "<ol><li>first</li><li>second</li></ol>", "&lt;script&gt;alert(1)&lt;/script&gt;"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "<script>") || strings.Contains(got, `href="javascript:`) {
		t.Fatalf("unsafe markup leaked: %s", got)
	}
}

func TestRepositoryMarkdownResolvesRelativeLinks(t *testing.T) {
	query := url.Values{"owner": {"alice"}, "repo": {"notes"}, "ref": {"refs/heads/main"}}
	got := string(renderRepositoryMarkdown("[guide](docs/guide.md) [up](../secret.md) [site](https://example.com)", query))
	if !strings.Contains(got, `href="/repo?owner=alice&amp;path=docs%2Fguide.md&amp;ref=refs%2Fheads%2Fmain&amp;repo=notes&amp;view=file"`) {
		t.Fatalf("relative link was not mapped to repository viewer: %s", got)
	}
	if !strings.Contains(got, `href="https://example.com"`) {
		t.Fatalf("repository link handling changed unsafe or external links: %s", got)
	}
}

func TestRepositoryMarkdownDecodesPathsAndPreservesFragments(t *testing.T) {
	query := url.Values{"owner": {"alice"}, "repo": {"notes"}, "ref": {"refs/heads/main"}}
	got := string(renderRepositoryMarkdown("[guide](docs/caf%C3%A9%20guide.md?download=1#L2)", query))
	want := `href="/repo?download=1&amp;owner=alice&amp;path=docs%2Fcaf%C3%A9+guide.md&amp;ref=refs%2Fheads%2Fmain&amp;repo=notes&amp;view=file#L2"`
	if !strings.Contains(got, want) {
		t.Fatalf("encoded README link lost its resource or fragment: %s", got)
	}
}

func TestChatMarkdownBreaksLinesAndAutolinksOutsideLinksAndCode(t *testing.T) {
	got := string(renderChatMarkdown("It works: **[the page](https://012.run/wiki/agents)**.\nsee https://example.com/x, and nostr:npub1ttrypewl3au52wqux86r22yt506c077k3maj02a0jste97wrvd5sjfutc2\n\n- `code https://not.a.link`\n- [x](javascript:alert(1))\n\n```\nhttps://in.code\n```"))
	for _, want := range []string{
		`<strong><a href="https://012.run/wiki/agents">the page</a></strong>.<br>see <a href="https://example.com/x" rel="noopener">https://example.com/x</a>, and <a href="/open?target=nostr%3Anpub1ttrypewl3au52wqux86r22yt506c077k3maj02a0jste97wrvd5sjfutc2">nostr:npub1ttrypewl3au52wqux86r22yt506c077k3maj02a0jste97wrvd5sjfutc2</a></p>`,
		"<li><code>code https://not.a.link</code></li>",
		"<li>[x](javascript:alert(1))</li>",
		"<pre><code>https://in.code</code></pre>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Count(got, "<a ") != 3 {
		t.Fatalf("expected three links, got %s", got)
	}
}

func TestMarkdownRendersTablesWithAlignmentAndInlineMarkdown(t *testing.T) {
	source := "| Name | Count | Notes |\n| :--- | ---: | :---: |\n| **Ada** | 3 | [docs](https://example.com) |\n| Grace |  | `a | b` |"
	got := string(renderMarkdown(source))
	for _, want := range []string{
		`<div data-markdown-table="" role="region" aria-label="Table" tabindex="0"><table>`,
		`<thead><tr><th scope="col" data-align="left">Name</th><th scope="col" data-align="right">Count</th><th scope="col" data-align="center">Notes</th></tr></thead>`,
		`<tbody><tr><td data-align="left"><strong>Ada</strong></td><td data-align="right">3</td><td data-align="center"><a href="https://example.com">docs</a></td></tr>`,
		`<tr><td data-align="left">Grace</td><td data-align="right"></td><td data-align="center"><code>a | b</code></td></tr></tbody></table></div>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
}

func TestMarkdownTableEscapesPipesAndLeavesMalformedInputAsText(t *testing.T) {
	got := string(renderMarkdown("a \\| b\n---\n\n| one | two |\n| --- |\n| three | four | five |"))
	if strings.Contains(got, `data-markdown-table`) {
		t.Fatalf("malformed table was rendered: %s", got)
	}
	if !strings.Contains(got, `a \| b ---`) || !strings.Contains(got, `| one | two |`) {
		t.Fatalf("malformed table was not preserved as text: %s", got)
	}

	got = string(renderMarkdown("a \\| b | c\n--- | ---\nvalue \\| kept | ok"))
	if !strings.Contains(got, `<th scope="col">a | b</th>`) || !strings.Contains(got, `<td>value | kept</td>`) {
		t.Fatalf("escaped pipe was split or escaped incorrectly: %s", got)
	}
}

func TestChatMarkdownAutolinksTableText(t *testing.T) {
	got := string(renderChatMarkdown("| Link |\n| --- |\n| https://example.com/path |"))
	if !strings.Contains(got, `<td><a href="https://example.com/path" rel="noopener">https://example.com/path</a></td>`) {
		t.Fatalf("table cell was not autolinked: %s", got)
	}
}

func TestMarkdownTableStopsBeforeHeadingsAndFences(t *testing.T) {
	got := string(renderMarkdown("| Name | Value |\n| --- | --- |\n| one | two |\n# | Heading |\n\n```sh | example\n| code |\n```"))
	if strings.Contains(got, "<td># | Heading |</td>") || strings.Contains(got, "<td>```sh | example</td>") {
		t.Fatalf("table consumed a following block: %s", got)
	}
	if !strings.Contains(got, "<h1>| Heading |</h1>") || !strings.Contains(got, "<pre><code data-lang=\"sh\">| code |</code></pre>") {
		t.Fatalf("following heading or fence was not rendered separately: %s", got)
	}
}

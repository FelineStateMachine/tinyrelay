package webui

import (
	"net/url"
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

package wiki

import (
	"fmt"
	"strings"
	"testing"
)

func TestNormalizeFollowsNIP54Examples(t *testing.T) {
	cases := map[string]string{
		"Wiki Article":      "wiki-article",
		"What's Up?":        "whats-up",
		"  Hello  World  ":  "hello-world",
		"Article 1":         "article-1",
		"ウィキペディア":           "ウィキペディア",
		"Ñoño":              "ñoño",
		"Москва":            "москва",
		"日本語 Article":       "日本語-article",
		"proof-of-work":     "proof-of-work",
		"--Lightning--":     "lightning",
		"a -- b\tc":         "a-b-c",
		"C++ & C#":          "c-c",
		"e\u0301lan":        "e\u0301lan",
		"":                  "",
		"!!!":               "",
		"snake_case_name":   "snake-case-name",
		"Tabs\tand\nbreaks": "tabs-and-breaks",
	}
	for input, want := range cases {
		if got := Normalize(input); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLinksFindWikilinksReferencesAndNostrTargets(t *testing.T) {
	content := "Bitcoin is a [cryptocurrency][] invented by [Satoshi Nakamoto][].\n\n" +
		"See also: [proof of work][] and [lightning network][Lightning Network], [[Mining|the miners]] and _[Биткойн][]_.\n\n" +
		"- [Bob](nostr:npub1bob) wrote `[[not a link]]`\n" +
		"- [web](https://example.com/[[x]])\n\n" +
		"```\n[[ignored in code]]\n```\n\n" +
		"[Satoshi Nakamoto]: nostr:npub1satoshi\n"
	got := Links(content)
	want := []string{"nostr:npub1satoshi", "cryptocurrency", "proof-of-work", "lightning-network", "mining", "биткойн", "nostr:npub1bob"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Links = %v, want %v", got, want)
	}
	if links := Links("plain text"); len(links) != 0 {
		t.Fatalf("plain text links = %v", links)
	}
}

func TestRenderHTMLSupportedSyntax(t *testing.T) {
	content := "# Title with _emphasis_\n\n" +
		"A paragraph with *strong*, `code <b>` and a [link](https://example.com/a?b=1&c=2).\n" +
		"Second line of the paragraph.\n\n" +
		"## Lists\n\n" +
		"- one [[Wiki Page]]\n- two [[Другая|shown]]\n\n" +
		"1. first\n2) second\n\n" +
		"``` go\nfunc main() {}\n<script>alert(1)</script>\n```\n\n" +
		"[Bob](nostr:npub1bob) and [reference][] and [defined][site].\n\n" +
		"[site]: https://example.org/\n"
	got := string(RenderHTML(content))
	for _, want := range []string{
		"<h1>Title with <em>emphasis</em></h1>",
		"<p>A paragraph with <strong>strong</strong>, <code>code &lt;b&gt;</code> and a <a href=\"https://example.com/a?b=1&amp;c=2\">link</a>.\nSecond line of the paragraph.</p>",
		"<h2>Lists</h2>",
		"<ul>\n<li>one <a href=\"/wiki/wiki-page\">Wiki Page</a></li>\n<li>two <a href=\"/wiki/%D0%B4%D1%80%D1%83%D0%B3%D0%B0%D1%8F\">shown</a></li>\n</ul>",
		"<ol>\n<li>first</li>\n<li>second</li>\n</ol>",
		"<pre><code data-lang=\"go\">func main() {}\n&lt;script&gt;alert(1)&lt;/script&gt;\n</code></pre>",
		"<a href=\"/open?target=nostr%3Anpub1bob\">Bob</a>",
		"<a href=\"/wiki/reference\">reference</a>",
		"<a href=\"https://example.org/\">defined</a>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "[site]") {
		t.Errorf("definition line rendered:\n%s", got)
	}
}

func TestRenderHTMLEscapesEverythingElse(t *testing.T) {
	cases := []struct{ content, forbidden, required string }{
		{"<script>alert(1)</script>", "<script", "&lt;script&gt;"},
		{"[click](javascript:alert(1))", "javascript:", "click"},
		{"[click](JavaScript:alert(1))", "avaScript:", "click"},
		{"[click](data:text/html;base64,AAAA)", "data:", "click"},
		{"[click](vbscript:msgbox)", "vbscript:", "click"},
		{"[click](//evil.example/x)", "evil.example", "click"},
		{"[click](/\\evil.example)", "href", "click"},
		{"[[Page]] [x](\"><img src=x onerror=alert(1)>)", "<img", "x&gt;"},
		{"[ref][]\n\n[ref]: javascript:alert(1)", "javascript:", "<p>ref</p>"},
		{"<a href=\"https://x\">raw html</a>", "<a href", "&lt;a href"},
		{"<img src=x onerror=alert(1)>", "<img", "&lt;img"},
		{"_<b>bold</b>_", "<b>", "<em>&lt;b&gt;bold&lt;/b&gt;</em>"},
		{"[[<script>]]", "<script", "/wiki/script"},
		{"[[x|<script>]]", "<script>", "&lt;script&gt;"},
		{"`<script>`", "<script>", "<code>&lt;script&gt;</code>"},
		{"[text](https://example.com/\"onmouseover=\"alert(1))", "onmouseover=\"", "&#34;onmouseover=&#34;"},
	}
	for _, tc := range cases {
		got := string(RenderHTML(tc.content))
		if strings.Contains(got, tc.forbidden) {
			t.Errorf("%q rendered %q:\n%s", tc.content, tc.forbidden, got)
		}
		if !strings.Contains(got, tc.required) {
			t.Errorf("%q lacks %q:\n%s", tc.content, tc.required, got)
		}
	}
}

func TestRenderHTMLHandlesEdgeCases(t *testing.T) {
	cases := map[string]string{
		"":                             "",
		"\n\n\n":                       "",
		"```":                          "<pre><code>\n</code></pre>\n",
		"```\nunterminated":            "<pre><code>unterminated\n</code></pre>\n",
		"#NoSpace":                     "<p>#NoSpace</p>\n",
		"####### seven":                "<p>####### seven</p>\n",
		"* not strong":                 "<ul>\n<li>not strong</li>\n</ul>\n",
		"a * b * c":                    "<p>a * b * c</p>\n",
		"a_b_c":                        "<p>a<em>b</em>c</p>\n",
		"\\*escaped\\*":                "<p>*escaped*</p>\n",
		"``code with ` inside``":       "<p><code>code with ` inside</code></p>\n",
		"[[]]":                         "<p>[[]]</p>\n",
		"[unclosed":                    "<p>[unclosed</p>\n",
		"[text](":                      "<p>[text](</p>\n",
		"[mailto](mailto:a@b.example)": "<p><a href=\"mailto:a@b.example\">mailto</a></p>\n",
		"[rel](/files)":                "<p><a href=\"/files\">rel</a></p>\n",
		"[bad](http://)":               "<p>bad</p>\n",
		"- item\nplain":                "<ul>\n<li>item</li>\n</ul>\n<p>plain</p>\n",
		"para\n- not a list":           "<p>para\n- not a list</p>\n",
		"Ünïcödé & <ampersand>":        "<p>Ünïcödé &amp; &lt;ampersand&gt;</p>\n",
		"line\r\nbreak":                "<p>line\nbreak</p>\n",
	}
	for content, want := range cases {
		if got := string(RenderHTML(content)); got != want {
			t.Errorf("RenderHTML(%q) =\n%s\nwant\n%s", content, got, want)
		}
	}
}

func TestRenderHTMLPipeTables(t *testing.T) {
	got := string(RenderHTML("Before.\n| Proven | Activity | Evidence |\n| --- | :---: | ---: |\n| yes | Discovery, *auth* | NIP-98 succeeded. |\n| no | Wiki fork \\| merge | remains |\nAfter."))
	for _, want := range []string{
		"<p>Before.</p>\n<table>\n<thead>\n<tr><th>Proven</th><th>Activity</th><th>Evidence</th></tr>\n</thead>\n<tbody>\n",
		"<tr><td>yes</td><td>Discovery, <strong>auth</strong></td><td>NIP-98 succeeded.</td></tr>\n",
		"<tr><td>no</td><td>Wiki fork | merge</td><td>remains</td></tr>\n</tbody>\n</table>\n<p>After.</p>\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	// A table without a separator is all body rows; a pipe inside a paragraph is text.
	got = string(RenderHTML("| a | b |\n| c | d |\n\ntext | with a pipe"))
	if !strings.Contains(got, "<table>\n<tbody>\n<tr><td>a</td><td>b</td></tr>\n<tr><td>c</td><td>d</td></tr>\n</tbody>\n</table>") || !strings.Contains(got, "<p>text | with a pipe</p>") {
		t.Fatalf("headerless table: %s", got)
	}
	if strings.Contains(string(RenderHTML("| <script>x</script> |\n| --- |")), "<script>") {
		t.Fatal("table cells were not escaped")
	}
}

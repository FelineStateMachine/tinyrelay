package views

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestBlocksFindFencesInOrderWithLanguages(t *testing.T) {
	content := "Intro\n\n```mermaid\ngraph TD;\n  A-->B;\n```\n\nText\n\n~~~\nplain\n~~~\n\n  ```` Python extra\nprint(1)\n```\nstill python\n````\n\n```dot\n"
	blocks := Blocks(content)
	if len(blocks) != 4 {
		t.Fatalf("blocks = %+v", blocks)
	}
	for i, want := range []Block{
		{0, "mermaid", "graph TD;\n  A-->B;"},
		{1, "", "plain"},
		{2, "python", "print(1)\n```\nstill python"},
		{3, "dot", ""},
	} {
		if blocks[i] != want {
			t.Errorf("block %d = %+v, want %+v", i, blocks[i], want)
		}
	}
	matched := Matching(blocks, []string{"mermaid", "dot"})
	if len(matched) != 2 || matched[0].Index != 0 || matched[1].Index != 3 {
		t.Fatalf("matching = %+v", matched)
	}
	if got := Matching(blocks, []string{"go"}); got != nil {
		t.Fatalf("unexpected match %+v", got)
	}
}

func TestFenceHelpersRejectShortAndMixedFences(t *testing.T) {
	if _, _, ok := OpenFence("``"); ok {
		t.Fatal("two backticks opened a fence")
	}
	if _, _, ok := OpenFence("text ```"); ok {
		t.Fatal("fence in the middle of a line opened a fence")
	}
	marker, lang, ok := OpenFence("~~~~ Mermaid title")
	if !ok || marker != "~~~~" || lang != "mermaid" {
		t.Fatalf("open = %q %q %v", marker, lang, ok)
	}
	if ClosesFence("~~~", marker) || ClosesFence("````", marker) || !ClosesFence("  ~~~~~ ", marker) {
		t.Fatal("closing fence rules")
	}
}

func TestHashCoversLanguageAndSource(t *testing.T) {
	sum := sha256.Sum256([]byte("mermaid\ngraph TD"))
	if Hash("mermaid", "graph TD") != hex.EncodeToString(sum[:]) {
		t.Fatal("hash differs from sha256(lang + newline + source)")
	}
	if Hash("mermaid", "graph TD") == Hash("dot", "graph TD") {
		t.Fatal("language is not part of the hash")
	}
	if Blocks("```mermaid\na\n```")[0].Source != Blocks("```mermaid\r\na\r\n```")[0].Source {
		t.Fatal("line endings change the source")
	}
}

func TestFigureEmbedsTheArtifactWithTheCodeAsFallback(t *testing.T) {
	got := Figure("diagrams", "mermaid", "graph TD;\n  A-->B & <C>;")
	hash := Hash("mermaid", "graph TD;\n  A-->B & <C>;")
	want := `<figure data-view="diagrams"><view-artifact><img src="/views/diagrams/` + hash + `" alt="Rendered mermaid block"><details><summary>Source</summary><pre><code data-lang="mermaid">graph TD;` + "\n" + `  A--&gt;B &amp; &lt;C&gt;;</code></pre></details></view-artifact></figure>` + "\n"
	if got != want {
		t.Fatalf("figure =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "class=") {
		t.Fatal("figure carries a class attribute")
	}
}

func TestRendererPicksTheFirstViewForALanguage(t *testing.T) {
	r := NewRenderer([]View{{Name: "first", Languages: []string{"mermaid"}}, {Name: "second", Languages: []string{"Mermaid", "dot"}}})
	if html, ok := r.Block("MERMAID", "x"); !ok || !strings.Contains(html, `data-view="first"`) {
		t.Fatalf("mermaid = %q %v", html, ok)
	}
	if html, ok := r.Block("dot", "x"); !ok || !strings.Contains(html, `data-view="second"`) {
		t.Fatalf("dot = %q %v", html, ok)
	}
	if _, ok := r.Block("go", "x"); ok {
		t.Fatal("go rendered")
	}
	if _, ok := (Renderer{}).Block("mermaid", "x"); ok {
		t.Fatal("empty renderer rendered")
	}
}

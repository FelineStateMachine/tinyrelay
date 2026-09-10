package views

import (
	"crypto/sha256"
	"encoding/hex"
	"html"
	"regexp"
	"strings"
)

// Block is one fenced code block. Index counts every fenced block in the
// text from zero, whether or not it carries a language.
type Block struct {
	Index  int    `json:"index"`
	Lang   string `json:"lang"`
	Source string `json:"source"`
}

// NamePattern is the shape of a custom view name.
var NamePattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// OpenFence reports whether a line opens a fenced code block: three or more
// backticks or tildes, with the language as the first word after them. The
// marker returned is the fence run, so the closing line must use the same
// character.
func OpenFence(line string) (marker, lang string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 3 || (trimmed[0] != '`' && trimmed[0] != '~') {
		return "", "", false
	}
	run := len(trimmed) - len(strings.TrimLeft(trimmed, trimmed[:1]))
	if run < 3 {
		return "", "", false
	}
	info := strings.Fields(trimmed[run:])
	if len(info) > 0 {
		lang = strings.ToLower(info[0])
	}
	return trimmed[:run], lang, true
}

// ClosesFence reports whether a line closes the fence opened with marker: a
// run of at least as many of the same character and nothing else.
func ClosesFence(line, marker string) bool {
	trimmed := strings.TrimSpace(line)
	return len(trimmed) >= len(marker) && strings.Trim(trimmed, marker[:1]) == ""
}

// Blocks lists the fenced code blocks of a text in order. The source is the
// lines between the fences joined by newlines, without a trailing newline.
func Blocks(content string) []Block {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var out []Block
	for i := 0; i < len(lines); i++ {
		marker, lang, ok := OpenFence(lines[i])
		if !ok {
			continue
		}
		var body []string
		for i++; i < len(lines) && !ClosesFence(lines[i], marker); i++ {
			body = append(body, lines[i])
		}
		out = append(out, Block{Index: len(out), Lang: lang, Source: strings.Join(body, "\n")})
	}
	return out
}

// Matching keeps the blocks whose language is one of the given languages.
func Matching(blocks []Block, languages []string) []Block {
	var out []Block
	for _, block := range blocks {
		if block.Lang != "" && contains(languages, block.Lang) {
			out = append(out, block)
		}
	}
	return out
}

// Hash names a block: the SHA-256 of the language, a newline and the
// source, in hex. The same block in two texts has the same hash.
func Hash(lang, source string) string {
	sum := sha256.Sum256([]byte(lang + "\n" + source))
	return hex.EncodeToString(sum[:])
}

// HashPattern is the shape of a block hash in a path.
var HashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Path is the address an artifact is served at.
func Path(view, hash, extension string) string {
	path := "/views/" + view + "/" + hash
	if extension != "" {
		path += "." + extension
	}
	return path
}

// Figure is the markup for a block a custom view renders. The image remains
// transparent and follows the page color scheme, while the native details
// disclosure keeps the source available without requiring script.
func Figure(view, lang, source string) string {
	var b strings.Builder
	alt := "Rendered view"
	if lang != "" {
		alt = "Rendered " + lang + " block"
	}
	b.WriteString(`<figure data-view="` + html.EscapeString(view) + `"><view-artifact><img src="` + Path(view, Hash(lang, source), "") + `" alt="` + html.EscapeString(alt) + `"><details><summary>Source</summary><pre><code`)
	if lang != "" {
		b.WriteString(` data-lang="` + html.EscapeString(lang) + `"`)
	}
	b.WriteString(">" + html.EscapeString(source) + "</code></pre></details></view-artifact></figure>\n")
	return b.String()
}

// Renderer decides how a fenced block is shown: the view that renders the
// language, if any. Languages maps a lowercase language to a view name.
type Renderer struct {
	Languages map[string]string
}

// NewRenderer indexes views by the languages they render. The first view
// that names a language renders it.
func NewRenderer(views []View) Renderer {
	index := map[string]string{}
	for _, view := range views {
		for _, lang := range view.Languages {
			lang = strings.ToLower(lang)
			if _, taken := index[lang]; !taken {
				index[lang] = view.Name
			}
		}
	}
	return Renderer{Languages: index}
}

// View is the summary a renderer needs: the name and the languages.
type View struct {
	Name      string   `json:"name"`
	Languages []string `json:"languages"`
}

// Block returns the figure markup for a block when a view renders its
// language.
func (r Renderer) Block(lang, source string) (string, bool) {
	view, ok := r.Languages[strings.ToLower(lang)]
	if !ok || view == "" {
		return "", false
	}
	return Figure(view, strings.ToLower(lang), source), true
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

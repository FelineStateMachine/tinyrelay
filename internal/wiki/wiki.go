// Package wiki implements the NIP-54 rules that do not depend on storage:
// article name normalization, link discovery and a conservative Djot
// renderer for article content.
package wiki

import (
	"html/template"
	"net/url"
	"strings"
	"unicode"
)

// Normalize turns a title into the article's d tag: lowercase, whitespace as
// hyphens, punctuation and symbols removed, hyphens collapsed and trimmed.
// Letters of every script, combining marks and digits are preserved.
func Normalize(title string) string {
	var out []rune
	for _, r := range strings.ToLower(title) {
		switch {
		case unicode.IsSpace(r) || r == '-' || r == '_':
			if len(out) > 0 && out[len(out)-1] != '-' {
				out = append(out, '-')
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r):
			out = append(out, r)
		}
	}
	return strings.Trim(string(out), "-")
}

// Links lists the targets referenced from Djot content in order of first
// appearance: normalized article names for wikilinks and reference-style
// links with no definition, and full nostr: URIs for NIP-21 links.
func Links(content string) []string {
	refs := definitions(content)
	seen := map[string]bool{}
	var links []string
	add := func(target string) {
		if target != "" && !seen[target] {
			seen[target] = true
			links = append(links, target)
		}
	}
	for _, def := range refs {
		if strings.HasPrefix(strings.ToLower(def), "nostr:") {
			add(def)
		}
	}
	var visit func(text string)
	visit = func(text string) {
		for _, span := range inlineSpans(text) {
			switch span.kind {
			case "em", "strong":
				visit(span.text)
			case "wikilink":
				add(Normalize(span.target))
			case "link":
				if strings.HasPrefix(strings.ToLower(span.target), "nostr:") {
					add(span.target)
				}
			case "reference":
				if def, ok := refs[strings.ToLower(span.target)]; ok {
					if strings.HasPrefix(strings.ToLower(def), "nostr:") {
						add(def)
					}
				} else {
					add(Normalize(span.target))
				}
			}
		}
	}
	for _, block := range blocks(content) {
		switch block.kind {
		case "pre", "def":
		case "ul", "ol":
			for _, item := range block.items {
				visit(item)
			}
		default:
			visit(block.text)
		}
	}
	return links
}

// RenderHTML renders a subset of Djot: ATX headings, paragraphs, fenced
// code, bullet and numbered lists, pipe tables, emphasis, strong, code spans,
// links, [[wikilinks]] and reference-style links. Everything else is shown as
// escaped text. Link targets are limited to http, https, mailto, nostr and
// site-relative paths.
func RenderHTML(content string) template.HTML {
	return RenderHTMLWith(content, Options{})
}

// Options adjusts rendering. Block, when set, is offered every fenced code
// block with its language and source and returns markup to show in its
// place, or false to keep the code block. The relay uses it for custom
// views without this package knowing about them.
type Options struct {
	Block func(lang, source string) (html string, ok bool)
}

// RenderHTMLWith renders Djot the way RenderHTML does, with the options
// applied.
func RenderHTMLWith(content string, options Options) template.HTML {
	refs := definitions(content)
	var b strings.Builder
	for _, block := range blocks(content) {
		switch block.kind {
		case "def":
			continue
		case "pre":
			if options.Block != nil {
				if html, ok := options.Block(block.lang, strings.TrimSuffix(block.text, "\n")); ok {
					b.WriteString(html)
					continue
				}
			}
			b.WriteString("<pre><code")
			if block.lang != "" {
				b.WriteString(` data-lang="` + template.HTMLEscapeString(block.lang) + `"`)
			}
			b.WriteString(">" + template.HTMLEscapeString(block.text) + "</code></pre>\n")
		case "heading":
			level := string(rune('0' + block.level))
			b.WriteString("<h" + level + ">" + renderInline(block.text, refs) + "</h" + level + ">\n")
		case "ul", "ol":
			b.WriteString("<" + block.kind + ">\n")
			for _, item := range block.items {
				b.WriteString("<li>" + renderInline(item, refs) + "</li>\n")
			}
			b.WriteString("</" + block.kind + ">\n")
		case "table":
			b.WriteString("<table>\n")
			for i, row := range block.rows {
				cell := "td"
				if i < block.level {
					cell = "th"
				}
				if i == 0 && block.level > 0 {
					b.WriteString("<thead>\n")
				}
				if i == block.level {
					if block.level > 0 {
						b.WriteString("</thead>\n")
					}
					b.WriteString("<tbody>\n")
				}
				b.WriteString("<tr>")
				for _, text := range row {
					b.WriteString("<" + cell + ">" + renderInline(text, refs) + "</" + cell + ">")
				}
				b.WriteString("</tr>\n")
			}
			if block.level >= len(block.rows) {
				b.WriteString("</thead>\n")
			} else {
				b.WriteString("</tbody>\n")
			}
			b.WriteString("</table>\n")
		default:
			b.WriteString("<p>" + renderInline(block.text, refs) + "</p>\n")
		}
	}
	return template.HTML(b.String())
}

type block struct {
	kind  string
	level int
	lang  string
	text  string
	items []string
	rows  [][]string // table rows; level counts the header rows
}

// definitions collects reference link definitions, "[label]: target", keyed
// by their lowercase label.
func definitions(content string) map[string]string {
	refs := map[string]string{}
	for _, block := range blocks(content) {
		if block.kind == "def" {
			refs[strings.ToLower(block.lang)] = block.text
		}
	}
	return refs
}

func blocks(content string) []block {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var out []block
	var para []string
	flush := func() {
		if len(para) > 0 {
			out = append(out, block{kind: "p", text: strings.Join(para, "\n")})
			para = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			flush()
			continue
		}
		if fence := trimmed[:min(3, len(trimmed))]; fence == "```" || fence == "~~~" {
			flush()
			run := len(trimmed) - len(strings.TrimLeft(trimmed, fence[:1]))
			lang := strings.ToLower(append(strings.Fields(trimmed[run:]), "")[0])
			var body []string
			for i++; i < len(lines); i++ {
				if t := strings.TrimSpace(lines[i]); len(t) >= run && strings.Trim(t, fence[:1]) == "" {
					break
				}
				body = append(body, lines[i])
			}
			out = append(out, block{kind: "pre", lang: lang, text: strings.Join(body, "\n") + "\n"})
			continue
		}
		if level := headingLevel(trimmed); level > 0 {
			flush()
			out = append(out, block{kind: "heading", level: level, text: strings.TrimSpace(trimmed[level:])})
			continue
		}
		if label, target, ok := definitionLine(trimmed); ok && len(para) == 0 {
			out = append(out, block{kind: "def", lang: label, text: target})
			continue
		}
		if tableRow(trimmed) {
			// A table interrupts a paragraph, the way Djot block starts do.
			flush()
			var rows [][]string
			header := 0
			for ; i < len(lines); i++ {
				current := strings.TrimSpace(lines[i])
				if !tableRow(current) {
					break
				}
				if separatorRow(current) {
					// A separator marks every row above it as a header.
					if header == 0 {
						header = len(rows)
					}
					continue
				}
				rows = append(rows, tableCells(current))
			}
			i--
			if len(rows) > 0 {
				out = append(out, block{kind: "table", level: header, rows: rows})
			}
			continue
		}
		if kind, _ := listMarker(trimmed); kind != "" && len(para) == 0 {
			var items []string
			for ; i < len(lines); i++ {
				current := strings.TrimSpace(lines[i])
				itemKind, rest := listMarker(current)
				if itemKind != kind {
					break
				}
				items = append(items, rest)
			}
			i--
			out = append(out, block{kind: kind, items: items})
			continue
		}
		para = append(para, trimmed)
	}
	flush()
	return out
}

// tableRow reports a Djot pipe table row: a line that starts and ends with
// a pipe.
func tableRow(line string) bool {
	return len(line) >= 2 && strings.HasPrefix(line, "|") && strings.HasSuffix(line, "|")
}

// separatorRow reports the header separator, cells of dashes with optional
// alignment colons, such as |---|:--:|.
func separatorRow(line string) bool {
	cells := tableCells(line)
	if len(cells) == 0 {
		return false
	}
	for _, cell := range cells {
		if strings.Trim(cell, "-") == "" && strings.Contains(cell, "-") {
			continue
		}
		if strings.Trim(strings.TrimSuffix(strings.TrimPrefix(cell, ":"), ":"), "-") == "" && strings.Contains(cell, "-") {
			continue
		}
		return false
	}
	return true
}

// tableCells splits a row into trimmed cells; a backslash escapes a pipe
// inside a cell.
func tableCells(line string) []string {
	inner := strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|")
	var cells []string
	var current strings.Builder
	for i := 0; i < len(inner); i++ {
		switch {
		case inner[i] == '\\' && i+1 < len(inner) && inner[i+1] == '|':
			current.WriteByte('|')
			i++
		case inner[i] == '|':
			cells = append(cells, strings.TrimSpace(current.String()))
			current.Reset()
		default:
			current.WriteByte(inner[i])
		}
	}
	cells = append(cells, strings.TrimSpace(current.String()))
	return cells
}

func headingLevel(line string) int {
	level := len(line) - len(strings.TrimLeft(line, "#"))
	if level == 0 || level > 6 || level >= len(line) || line[level] != ' ' {
		return 0
	}
	return level
}

func definitionLine(line string) (label, target string, ok bool) {
	end := strings.Index(line, "]:")
	if !strings.HasPrefix(line, "[") || end < 1 {
		return "", "", false
	}
	target = strings.TrimSpace(line[end+2:])
	return strings.TrimSpace(line[1:end]), target, target != "" && !strings.ContainsAny(target, " \t")
}

func listMarker(line string) (kind, rest string) {
	if len(line) > 1 && (line[0] == '-' || line[0] == '*' || line[0] == '+') && line[1] == ' ' {
		return "ul", strings.TrimSpace(line[2:])
	}
	digits := len(line) - len(strings.TrimLeft(line, "0123456789"))
	if digits > 0 && digits+1 < len(line) && (line[digits] == '.' || line[digits] == ')') && line[digits+1] == ' ' {
		return "ol", strings.TrimSpace(line[digits+2:])
	}
	return "", ""
}

type span struct {
	kind         string // text, code, em, strong, link, wikilink, reference
	text, target string
}

// inlineSpans splits paragraph text into literal runs and recognized inline
// elements. It never nests: the text inside a link or emphasis is rendered
// as further spans by the caller.
func inlineSpans(text string) []span {
	var spans []span
	var literal strings.Builder
	flush := func() {
		if literal.Len() > 0 {
			spans = append(spans, span{kind: "text", text: literal.String()})
			literal.Reset()
		}
	}
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case c == '\\' && i+1 < len(text):
			literal.WriteByte(text[i+1])
			i += 2
			continue
		case c == '`':
			run := strings.TrimLeft(text[i:], "`")
			marker := text[i : len(text)-len(run)]
			if end := strings.Index(run, marker); end >= 0 {
				flush()
				spans = append(spans, span{kind: "code", text: strings.TrimSpace(run[:end])})
				i += 2*len(marker) + end
				continue
			}
		case c == '*' || c == '_':
			if end := emphasisEnd(text, i); end > 0 {
				flush()
				kind := "em"
				if c == '*' {
					kind = "strong"
				}
				spans = append(spans, span{kind: kind, text: text[i+1 : end]})
				i = end + 1
				continue
			}
		case c == '[' && strings.HasPrefix(text[i:], "[["):
			if end := strings.Index(text[i:], "]]"); end > 2 {
				flush()
				target, label, _ := strings.Cut(text[i+2:i+end], "|")
				spans = append(spans, span{kind: "wikilink", target: strings.TrimSpace(target), text: strings.TrimSpace(label)})
				i += end + 2
				continue
			}
		case c == '[':
			if s, n := linkAt(text[i:]); n > 0 {
				flush()
				spans = append(spans, s)
				i += n
				continue
			}
		}
		literal.WriteByte(c)
		i++
	}
	flush()
	return spans
}

// emphasisEnd finds the closing marker for Djot emphasis: the opener is not
// followed by whitespace and the closer is not preceded by it.
func emphasisEnd(text string, start int) int {
	marker := text[start]
	if start+1 >= len(text) || text[start+1] == ' ' || text[start+1] == '\n' {
		return 0
	}
	for i := start + 2; i < len(text); i++ {
		if text[i] == marker && text[i-1] != ' ' && text[i-1] != '\n' {
			return i
		}
	}
	return 0
}

// linkAt recognizes [text](target), [text][label] and [text][] at the start
// of the string and reports how many bytes it consumed.
func linkAt(text string) (span, int) {
	depth, end := 0, -1
	for i := 0; i < len(text); i++ {
		if text[i] == '[' {
			depth++
		} else if text[i] == ']' {
			if depth--; depth == 0 {
				end = i
				break
			}
		}
	}
	if end <= 0 {
		return span{}, 0
	}
	label := text[1:end]
	rest := text[end+1:]
	if strings.HasPrefix(rest, "(") {
		if close := strings.IndexByte(rest, ')'); close > 0 {
			return span{kind: "link", text: label, target: strings.TrimSpace(rest[1:close])}, end + 2 + close
		}
	}
	if strings.HasPrefix(rest, "[") {
		if close := strings.IndexByte(rest, ']'); close > 0 {
			target := strings.TrimSpace(rest[1:close])
			if target == "" {
				target = label
			}
			return span{kind: "reference", text: label, target: target}, end + 2 + close
		}
	}
	return span{}, 0
}

func renderInline(text string, refs map[string]string) string {
	var b strings.Builder
	for _, s := range inlineSpans(text) {
		switch s.kind {
		case "text":
			b.WriteString(template.HTMLEscapeString(s.text))
		case "code":
			b.WriteString("<code>" + template.HTMLEscapeString(s.text) + "</code>")
		case "em", "strong":
			b.WriteString("<" + s.kind + ">" + renderInline(s.text, refs) + "</" + s.kind + ">")
		case "wikilink":
			label := s.text
			if label == "" {
				label = s.target
			}
			b.WriteString(anchor("/wiki/"+url.PathEscape(Normalize(s.target)), renderInline(label, refs)))
		case "link":
			b.WriteString(anchor(safeHref(s.target), renderInline(s.text, refs)))
		case "reference":
			href := "/wiki/" + url.PathEscape(Normalize(s.target))
			if def, ok := refs[strings.ToLower(s.target)]; ok {
				href = safeHref(def)
			}
			b.WriteString(anchor(href, renderInline(s.text, refs)))
		}
	}
	return b.String()
}

func anchor(href, inner string) string {
	if href == "" {
		return inner
	}
	return `<a href="` + template.HTMLEscapeString(href) + `">` + inner + `</a>`
}

// safeHref keeps http, https, mailto and site-relative targets, routes
// nostr: links through the relay's open page and drops everything else.
func safeHref(target string) string {
	target = strings.TrimSpace(target)
	lower := strings.ToLower(target)
	switch {
	case strings.HasPrefix(lower, "nostr:"):
		return "/open?target=" + url.QueryEscape(target)
	case strings.HasPrefix(lower, "mailto:"):
		return target
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		if u, err := url.Parse(target); err == nil && u.Host != "" {
			return target
		}
	case strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "//") && !strings.HasPrefix(target, "/\\"):
		return target
	}
	return ""
}

package webui

import (
	"html"
	"html/template"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const sourcePreviewLimit = 256 * 1024
const sourcePreviewLines = 10000

// A byte bound alone permits hundreds of thousands of empty table rows.
// Bound the preview markup separately; raw downloads remain complete.
func previewLines(content string) ([]string, bool) {
	if content == "" {
		return nil, false
	}
	content = strings.TrimSuffix(content, "\n")
	lines := strings.SplitN(content, "\n", sourcePreviewLines+1)
	if len(lines) > sourcePreviewLines {
		return lines[:sourcePreviewLines], true
	}
	return lines, false
}

// sourceHTML renders a bounded source preview. It returns template.HTML only
// after escaping every source byte and constructing the surrounding markup.
// The template should place the result inside a source-view region.
func sourceHTML(value any) template.HTML {
	m := valueMap(value)
	content, _ := m["content"].(string)
	name, _ := m["path"].(string)
	if name == "" {
		name, _ = m["name"].(string)
	}
	language := sourceLanguage(name)
	providedTruncated, _ := m["truncated"].(bool)
	truncated := providedTruncated || len(content) > sourcePreviewLimit
	if truncated {
		content = boundedContent(content)
	}
	lines, linesTruncated := previewLines(content)
	var out strings.Builder
	out.Grow(len(content) + len(lines)*120)
	out.WriteString(`<table class="source-view source-lang-`)
	out.WriteString(language)
	out.WriteString(`"><tbody>`)
	state := highlightState{}
	for number, line := range lines {
		out.WriteString(`<tr id="L`)
		writeUint(&out, number+1)
		out.WriteString(`"><td class="source-line"><a href="#L`)
		writeUint(&out, number+1)
		out.WriteString(`">`)
		writeUint(&out, number+1)
		out.WriteString(`</a></td><td class="source-code"><code>`)
		highlightLine(&out, line, language, &state)
		out.WriteString(`</code></td></tr>`)
	}
	if truncated || linesTruncated {
		out.WriteString(`<tr class="source-truncated"><td colspan="2">Preview truncated (256 KiB or 10,000 lines). Use the raw/download control to view the complete file.</td></tr>`)
	}
	out.WriteString(`</tbody></table>`)
	return template.HTML(out.String())
}

// diffHTML renders a line-oriented diff without interpreting its content.
func diffHTML(value any) template.HTML {
	m := valueMap(value)
	diff, _ := m["diff"].(string)
	if diff == "" {
		if text, ok := value.(string); ok {
			diff = text
		}
	}
	providedTruncated, _ := m["truncated"].(bool)
	truncated := providedTruncated || len(diff) > sourcePreviewLimit
	if truncated {
		diff = boundedContent(diff)
	}
	lines, linesTruncated := previewLines(diff)
	var out strings.Builder
	out.Grow(len(diff) + len(lines)*140)
	out.WriteString(`<table class="diff-view"><tbody>`)
	for number, line := range lines {
		class := "diff-context"
		switch {
		case strings.HasPrefix(line, "@@"):
			class = "diff-hunk"
		case strings.HasPrefix(line, "+"):
			class = "diff-add"
		case strings.HasPrefix(line, "-"):
			class = "diff-remove"
		case strings.HasPrefix(line, "\\"):
			class = "diff-note"
		}
		out.WriteString(`<tr class="`)
		out.WriteString(class)
		out.WriteString(`" id="D`)
		writeUint(&out, number+1)
		out.WriteString(`"><td class="diff-line"><a href="#D`)
		writeUint(&out, number+1)
		out.WriteString(`">`)
		writeUint(&out, number+1)
		out.WriteString(`</a></td><td class="diff-code"><code>`)
		out.WriteString(html.EscapeString(line))
		out.WriteString(`</code></td></tr>`)
	}
	if truncated || linesTruncated {
		out.WriteString(`<tr class="diff-truncated"><td colspan="2">Diff preview truncated (256 KiB or 10,000 lines).</td></tr>`)
	}
	out.WriteString(`</tbody></table>`)
	return template.HTML(out.String())
}

func boundedContent(content string) string {
	if len(content) <= sourcePreviewLimit {
		return content
	}
	content = content[:sourcePreviewLimit]
	for len(content) > 0 && !utf8.ValidString(content) {
		content = content[:len(content)-1]
	}
	return content
}

func sourceLanguage(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go":
		return "go"
	case ".js", ".jsx":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".css":
		return "css"
	case ".html", ".htm":
		return "html"
	case ".json":
		return "json"
	case ".md", ".markdown":
		return "markdown"
	case ".sh", ".bash":
		return "shell"
	case ".py":
		return "python"
	case ".sql":
		return "sql"
	default:
		return "text"
	}
}

type highlightState struct{ blockComment bool }

func highlightLine(out *strings.Builder, line, language string, state *highlightState) {
	for len(line) > 0 {
		if state.blockComment {
			end := strings.Index(line, "*/")
			if end < 0 {
				writeToken(out, "comment", line)
				return
			}
			writeToken(out, "comment", line[:end+2])
			line = line[end+2:]
			state.blockComment = false
			continue
		}
		if strings.HasPrefix(line, "/*") {
			end := strings.Index(line[2:], "*/")
			if end < 0 {
				state.blockComment = true
				writeToken(out, "comment", line)
				return
			}
			end += 4
			writeToken(out, "comment", line[:end])
			line = line[end:]
			continue
		}
		if prefix := commentPrefix(line, language); prefix != "" {
			writeToken(out, "comment", line)
			return
		}
		if line[0] == '\'' || line[0] == '"' || line[0] == '`' {
			quote := line[0]
			end := quotedEnd(line, quote)
			writeToken(out, "string", line[:end])
			line = line[end:]
			continue
		}
		value, size := utf8.DecodeRuneInString(line)
		if isWordStart(value) {
			end := size
			for end < len(line) {
				next, width := utf8.DecodeRuneInString(line[end:])
				if !isWordPart(next) {
					break
				}
				end += width
			}
			word := line[:end]
			if keyword(language, word) {
				writeToken(out, "keyword", word)
			} else {
				writeToken(out, "", word)
			}
			line = line[end:]
			continue
		}
		if unicode.IsDigit(value) {
			end := size
			for end < len(line) {
				next, width := utf8.DecodeRuneInString(line[end:])
				if !unicode.IsDigit(next) && !strings.ContainsRune("._", next) {
					break
				}
				end += width
			}
			writeToken(out, "number", line[:end])
			line = line[end:]
			continue
		}
		writeToken(out, "", line[:size])
		line = line[size:]
	}
}

func writeToken(out *strings.Builder, class, value string) {
	if class != "" {
		out.WriteString(`<span class="tok-`)
		out.WriteString(class)
		out.WriteString(`">`)
	}
	out.WriteString(html.EscapeString(value))
	if class != "" {
		out.WriteString(`</span>`)
	}
}

func quotedEnd(line string, quote byte) int {
	for index := 1; index < len(line); index++ {
		if line[index] == '\\' {
			index++
			continue
		}
		if line[index] == quote {
			return index + 1
		}
	}
	return len(line)
}

func commentPrefix(line, language string) string {
	if strings.HasPrefix(line, "//") || strings.HasPrefix(line, "#") {
		return line[:1]
	}
	if language == "sql" && strings.HasPrefix(line, "--") {
		return "--"
	}
	return ""
}

func isWordStart(value rune) bool { return unicode.IsLetter(value) || value == '_' }
func isWordPart(value rune) bool {
	return unicode.IsLetter(value) || unicode.IsDigit(value) || value == '_'
}

func keyword(language, word string) bool {
	switch language {
	case "go":
		return containsWord(word, "package import func type struct interface var const if else for range return go defer switch case select chan map")
	case "javascript", "typescript":
		return containsWord(word, "const let var function return if else for class interface type import export async await")
	case "python":
		return containsWord(word, "def class return if else for import from in with")
	default:
		return false
	}
}

func containsWord(word, list string) bool {
	for _, candidate := range strings.Fields(list) {
		if candidate == word {
			return true
		}
	}
	return false
}

func writeUint(out *strings.Builder, value int) {
	if value == 0 {
		out.WriteByte('0')
		return
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	out.Write(digits[index:])
}

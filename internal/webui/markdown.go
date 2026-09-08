package webui

import (
	"html"
	"html/template"
	"regexp"
	"strings"
)

// renderMarkdown turns a small, common subset of Markdown into HTML: ATX
// headings, paragraphs, fenced code, unordered and ordered lists, inline
// code, emphasis, and links. Every piece of text is escaped first and only
// the tags this function emits reach the page, so untrusted README files and
// relay descriptions cannot inject markup. Anything else stays plain text.
func renderMarkdown(source string) template.HTML {
	lines := strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n")
	var out strings.Builder
	var paragraph []string
	flush := func() {
		if len(paragraph) == 0 {
			return
		}
		out.WriteString("<p>" + inlineMarkdown(strings.Join(paragraph, " ")) + "</p>\n")
		paragraph = paragraph[:0]
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "```"):
			flush()
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(lines[i], "```"); i++ {
				code = append(code, lines[i])
			}
			out.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>\n")
		case strings.HasPrefix(line, "#"):
			flush()
			level := len(line) - len(strings.TrimLeft(line, "#"))
			if level > 6 {
				level = 6
			}
			text := strings.TrimSpace(line[level:])
			tag := "h" + string(rune('0'+level))
			out.WriteString("<" + tag + ">" + inlineMarkdown(text) + "</" + tag + ">\n")
		case isListItem(line):
			flush()
			ordered := orderedItem.MatchString(line)
			tag := "ul"
			if ordered {
				tag = "ol"
			}
			out.WriteString("<" + tag + ">")
			for ; i < len(lines) && isListItem(lines[i]); i++ {
				out.WriteString("<li>" + inlineMarkdown(listText(lines[i])) + "</li>")
			}
			i--
			out.WriteString("</" + tag + ">\n")
		case strings.TrimSpace(line) == "":
			flush()
		default:
			paragraph = append(paragraph, strings.TrimSpace(line))
		}
	}
	flush()
	return template.HTML(out.String())
}

var (
	orderedItem = regexp.MustCompile(`^\s*\d+[.)]\s+`)
	bulletItem  = regexp.MustCompile(`^\s*[-*+]\s+`)
	codeSpan    = regexp.MustCompile("`([^`]+)`")
	linkSpan    = regexp.MustCompile(`\[([^\]]+)\]\(((?:https?://|/|\./|#|mailto:)[^)\s]*)\)`)
	strongSpan  = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	emSpan      = regexp.MustCompile(`(^|[^*\w])\*([^*]+)\*`)
)

func isListItem(line string) bool {
	return bulletItem.MatchString(line) || orderedItem.MatchString(line)
}

func listText(line string) string {
	if loc := bulletItem.FindStringIndex(line); loc != nil {
		return line[loc[1]:]
	}
	return line[orderedItem.FindStringIndex(line)[1]:]
}

// inlineMarkdown escapes text, then restores the few inline forms it
// recognises. Link targets are restricted to http, https, relative paths,
// fragments and mailto so scripts and data URLs stay text.
func inlineMarkdown(text string) string {
	escaped := html.EscapeString(text)
	escaped = codeSpan.ReplaceAllString(escaped, "<code>$1</code>")
	escaped = linkSpan.ReplaceAllString(escaped, `<a href="$2">$1</a>`)
	escaped = strongSpan.ReplaceAllString(escaped, "<strong>$1</strong>")
	escaped = emSpan.ReplaceAllString(escaped, "$1<em>$2</em>")
	return escaped
}

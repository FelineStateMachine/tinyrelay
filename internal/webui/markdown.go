package webui

import (
	"html"
	"html/template"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/views"
)

// blockRenderer decides how a fenced code block is shown. It returns the
// markup for blocks a custom view renders and false for the rest, which
// stay code blocks.
type blockRenderer func(lang, source string) (string, bool)

// renderMarkdown turns a small, common subset of Markdown into HTML: ATX
// headings, paragraphs, fenced code, unordered and ordered lists, inline
// code, emphasis, and links. Every piece of text is escaped first and only
// the tags this function emits reach the page, so untrusted README files and
// relay descriptions cannot inject markup. Anything else stays plain text.
func renderMarkdown(source string) template.HTML {
	return markdownHTML(source, false, nil)
}

// renderMarkdownWith is renderMarkdown with fenced blocks offered to a
// custom view renderer first.
func renderMarkdownWith(source string, blocks blockRenderer) template.HTML {
	return markdownHTML(source, false, blocks)
}

// renderChatMarkdown is the same subset for chat messages, where a single
// newline is a line break and bare http(s) and nostr: references become
// links, as people and agents type them.
func renderChatMarkdown(source string) template.HTML {
	return renderChatMarkdownWith(source, nil)
}

func renderChatMarkdownWith(source string, blocks blockRenderer) template.HTML {
	return template.HTML(autolinkOutsideTags(string(markdownHTML(source, true, blocks))))
}

func markdownHTML(source string, breaks bool, blocks blockRenderer) template.HTML {
	lines := strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n")
	var out strings.Builder
	var paragraph []string
	flush := func() {
		if len(paragraph) == 0 {
			return
		}
		joiner := " "
		if breaks {
			joiner = "<br>"
		}
		parts := make([]string, len(paragraph))
		for i, line := range paragraph {
			parts[i] = inlineMarkdown(line)
		}
		out.WriteString("<p>" + strings.Join(parts, joiner) + "</p>\n")
		paragraph = paragraph[:0]
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		marker, lang, fenced := views.OpenFence(line)
		switch {
		case fenced:
			flush()
			var code []string
			for i++; i < len(lines) && !views.ClosesFence(lines[i], marker); i++ {
				code = append(code, lines[i])
			}
			body := strings.Join(code, "\n")
			if blocks != nil {
				if figure, ok := blocks(lang, body); ok {
					out.WriteString(figure)
					continue
				}
			}
			out.WriteString("<pre><code")
			if lang != "" {
				out.WriteString(` data-lang="` + html.EscapeString(lang) + `"`)
			}
			out.WriteString(">" + html.EscapeString(body) + "</code></pre>\n")
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
	linkSpan    = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
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
	escaped = linkSpan.ReplaceAllStringFunc(escaped, func(value string) string {
		match := linkSpan.FindStringSubmatch(value)
		if len(match) != 3 || !safeMarkdownLink(html.UnescapeString(match[2])) {
			return value
		}
		return `<a href="` + match[2] + `">` + match[1] + `</a>`
	})
	escaped = strongSpan.ReplaceAllString(escaped, "<strong>$1</strong>")
	escaped = emSpan.ReplaceAllString(escaped, "$1<em>$2</em>")
	return escaped
}

func safeMarkdownLink(target string) bool {
	return strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "http://") ||
		(strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "//")) ||
		strings.HasPrefix(target, "./") || strings.HasPrefix(target, "../") ||
		strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") ||
		(!strings.Contains(target, ":") && !strings.HasPrefix(target, "//"))
}

var markdownAnchor = regexp.MustCompile(`<a href="([^"]+)">`)

// renderRepositoryMarkdown resolves relative README links to relay file
// viewers while retaining external links and fragment-only links.
func renderRepositoryMarkdown(source string, query url.Values) template.HTML {
	return renderRepositoryMarkdownWith(source, query, nil)
}

func renderRepositoryMarkdownWith(source string, query url.Values, blocks blockRenderer) template.HTML {
	rendered := string(renderMarkdownWith(source, blocks))
	return template.HTML(markdownAnchor.ReplaceAllStringFunc(rendered, func(anchor string) string {
		match := markdownAnchor.FindStringSubmatch(anchor)
		target, err := url.Parse(html.UnescapeString(match[1]))
		if err != nil || target.IsAbs() || target.Host != "" || target.Path == "" || strings.HasPrefix(target.Path, "/") {
			return anchor
		}
		if target.Path == ".." || strings.HasPrefix(target.Path, "../") {
			return anchor
		}
		name := strings.TrimPrefix(path.Clean("/"+target.Path), "/")
		if name == "" {
			return anchor
		}
		destination, err := url.Parse(repoURL(query, "file", name))
		if err != nil {
			return anchor
		}
		params := destination.Query()
		for key, values := range target.Query() {
			if !params.Has(key) {
				params[key] = values
			}
		}
		destination.RawQuery = params.Encode()
		destination.Fragment = target.Fragment
		return `<a href="` + html.EscapeString(destination.String()) + `">`
	}))
}

// tagOrText splits rendered HTML into tags and the text between them.
var tagOrText = regexp.MustCompile(`<[^>]+>|[^<]+`)

// chatLinkPattern matches bare links in escaped text: http(s) URLs and
// nostr: references, as roomLinkPattern does before escaping.
var chatLinkPattern = regexp.MustCompile(`https?://[^\s<>"']+|(?:web\+)?nostr:[a-z0-9]+`)

// autolinkOutsideTags links bare URLs and nostr: references in the text
// between tags, leaving anything already inside a link or code alone.
func autolinkOutsideTags(rendered string) string {
	var out strings.Builder
	skip := 0
	for _, part := range tagOrText.FindAllString(rendered, -1) {
		if strings.HasPrefix(part, "<") {
			lower := strings.ToLower(part)
			switch {
			case strings.HasPrefix(lower, "<a ") || strings.HasPrefix(lower, "<code") || strings.HasPrefix(lower, "<pre") || strings.HasPrefix(lower, "<object"):
				skip++
			case strings.HasPrefix(lower, "</a") || strings.HasPrefix(lower, "</code") || strings.HasPrefix(lower, "</pre") || strings.HasPrefix(lower, "</object"):
				if skip > 0 {
					skip--
				}
			}
			out.WriteString(part)
			continue
		}
		if skip > 0 {
			out.WriteString(part)
			continue
		}
		last := 0
		for _, match := range chatLinkPattern.FindAllStringIndex(part, -1) {
			out.WriteString(part[last:match[0]])
			link := part[match[0]:match[1]]
			trimmed := strings.TrimRight(link, ".,;:!?)")
			href := trimmed
			if !strings.HasPrefix(trimmed, "http") {
				href = "/open?target=" + template.URLQueryEscaper(html.UnescapeString(trimmed))
			}
			out.WriteString(`<a href="` + html.EscapeString(html.UnescapeString(href)) + `"`)
			if strings.HasPrefix(trimmed, "http") {
				out.WriteString(` rel="noopener"`)
			}
			out.WriteString(`>` + trimmed + `</a>` + link[len(trimmed):])
			last = match[1]
		}
		out.WriteString(part[last:])
	}
	return out.String()
}

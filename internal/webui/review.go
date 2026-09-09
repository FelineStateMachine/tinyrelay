package webui

import (
	"html/template"
	"net/url"
	"strconv"
	"strings"
)

// Review rendering for the pull request page. The diff is split into rows
// that know their file and their line number on each side, so a comment
// carrying file and line tags can be placed under the row it names and every
// row can offer a link that opens the compose with that line preset.

// diffLine is one row of a unified diff. Old and New are the line numbers on
// each side of the hunk the row belongs to; a row outside a hunk, or on the
// side it does not touch, has 0 there.
type diffLine struct {
	Number  int
	Text    string
	Wrapper string
	File    string
	Old     int
	New     int
}

// diffLines parses rendered diff text into rows. Header lines keep the ins
// and del markers of the plain renderer; only hunk bodies are numbered.
func diffLines(lines []string) []diffLine {
	out := make([]diffLine, 0, len(lines))
	file := ""
	old, next := 0, 0
	inHunk := false
	for index, line := range lines {
		row := diffLine{Number: index + 1, Text: line}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHunk, file = false, ""
		case strings.HasPrefix(line, "@@"):
			inHunk = true
			old, next = hunkStarts(line)
			row.Wrapper = "b"
		case !inHunk && strings.HasPrefix(line, "+++ "):
			row.Wrapper = "ins"
			if path := diffPath(line[4:], "b/"); path != "" {
				file = path
			}
		case !inHunk && strings.HasPrefix(line, "--- "):
			row.Wrapper = "del"
			if path := diffPath(line[4:], "a/"); path != "" && file == "" {
				file = path
			}
		case inHunk && strings.HasPrefix(line, "+"):
			row.Wrapper, row.New = "ins", next
			next++
		case inHunk && strings.HasPrefix(line, "-"):
			row.Wrapper, row.Old = "del", old
			old++
		case inHunk && strings.HasPrefix(line, "\\"):
			// "\ No newline at end of file" belongs to no line.
		case inHunk:
			row.Old, row.New = old, next
			old++
			next++
		case strings.HasPrefix(line, "+"):
			row.Wrapper = "ins"
		case strings.HasPrefix(line, "-"):
			row.Wrapper = "del"
		}
		row.File = file
		out = append(out, row)
	}
	return out
}

// hunkStarts reads the old and new starting line numbers of a hunk header
// such as "@@ -12,7 +12,8 @@".
func hunkStarts(header string) (int, int) {
	fields := strings.Fields(header)
	if len(fields) < 3 {
		return 0, 0
	}
	start := func(value, prefix string) int {
		value = strings.TrimPrefix(value, prefix)
		if comma := strings.IndexByte(value, ','); comma >= 0 {
			value = value[:comma]
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return 0
		}
		return n
	}
	return start(fields[1], "-"), start(fields[2], "+")
}

// diffPath reads the path from a "--- a/path" or "+++ b/path" header. Git
// quotes paths with unusual characters; /dev/null means no file on that side.
func diffPath(value, prefix string) string {
	value = strings.TrimSpace(value)
	if tab := strings.IndexByte(value, '\t'); tab >= 0 {
		value = value[:tab]
	}
	if strings.HasPrefix(value, `"`) {
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
	}
	if value == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(value, prefix)
}

// reviewLine is one diff row ready for the template: its escaped markup,
// the link that starts a comment on it and the comments already anchored to
// it, oldest first.
type reviewLine struct {
	diffLine
	HTML       template.HTML
	CommentURL string
	Comments   []any
}

// reviewDiff is the pull request diff with its anchored comments placed.
// Thread holds the replies that are not shown inline, so the conversation
// below the diff lists each comment once.
type reviewDiff struct {
	Available bool
	Truncated bool
	Lines     []reviewLine
	Thread    []any
}

// reviewDiffView builds the diff for a pull request page. A signed-in
// viewer gets a comment link on each numbered line. Anchored comments whose
// line is not in the diff stay in the thread with their anchor shown.
func reviewDiffView(value any, query url.Values, actor string) reviewDiff {
	m := valueMap(value)
	replies := collaborationReplies(value)
	view := reviewDiff{Thread: replies}
	diff, _ := m["diff"].(string)
	if diff == "" {
		return view
	}
	view.Available = true
	providedTruncated, _ := m["diff_truncated"].(bool)
	view.Truncated = providedTruncated || len(diff) > sourcePreviewLimit
	if view.Truncated {
		diff = boundedContent(diff)
	}
	lines, linesTruncated := previewLines(diff)
	view.Truncated = view.Truncated || linesTruncated
	rows := diffLines(lines)
	view.Lines = make([]reviewLine, 0, len(rows))
	index := make(map[string]int, len(rows))
	for _, row := range rows {
		var markup strings.Builder
		writeWrapped(&markup, row.Wrapper, row.Text)
		line := reviewLine{diffLine: row, HTML: template.HTML(markup.String())}
		if row.File != "" {
			if row.New > 0 {
				index[reviewKey(row.File, "new", row.New)] = len(view.Lines)
			}
			if row.Old > 0 {
				index[reviewKey(row.File, "old", row.Old)] = len(view.Lines)
			}
			if actor != "" && (row.New > 0 || row.Old > 0) {
				line.CommentURL = reviewCommentURL(query, row)
			}
		}
		view.Lines = append(view.Lines, line)
	}
	view.Thread = view.Thread[:0]
	placed := make(map[int][]any)
	for _, reply := range replies {
		r := valueMap(reply)
		file := plainString(r["file"])
		lineNumber := int(unixSeconds(r["line"]))
		side := plainString(r["side"])
		at, ok := index[reviewKey(file, side, lineNumber)]
		if file == "" || lineNumber < 1 || !ok {
			view.Thread = append(view.Thread, reply)
			continue
		}
		placed[at] = append(placed[at], reply)
	}
	for at, comments := range placed {
		// Replies arrive newest first; read a line's comments in order.
		for left, right := 0, len(comments)-1; left < right; left, right = left+1, right-1 {
			comments[left], comments[right] = comments[right], comments[left]
		}
		view.Lines[at].Comments = comments
	}
	return view
}

func reviewKey(file, side string, line int) string {
	return side + ":" + strconv.Itoa(line) + ":" + file
}

// reviewCommentURL opens the pull request page with the compose preset to
// one line. Added and context lines name the new side; removed lines the old.
func reviewCommentURL(query url.Values, row diffLine) string {
	id := query.Get("id")
	if id == "" {
		id = query.Get("path")
	}
	side, line := "new", row.New
	if line == 0 {
		side, line = "old", row.Old
	}
	values := url.Values{"file": {row.File}, "line": {strconv.Itoa(line)}, "side": {side}}
	return repoURL(query, "pr", id) + "&" + values.Encode() + "#reply-compose"
}

// reviewAnchorQuery reads the line a compose should be preset to from the
// page query. It returns nil unless every part is valid.
func reviewAnchorQuery(query url.Values) map[string]any {
	if repoView(query) != "pr" {
		return nil
	}
	file, side := query.Get("file"), query.Get("side")
	line, err := strconv.Atoi(query.Get("line"))
	if strings.TrimSpace(file) == "" || err != nil || line < 1 || (side != "old" && side != "new") {
		return nil
	}
	return map[string]any{"File": file, "Line": line, "Side": side}
}

// reviewAnchorLabel describes a reply's anchor for the thread, or returns
// an empty string for an unanchored reply.
func reviewAnchorLabel(reply any) string {
	r := valueMap(reply)
	file := plainString(r["file"])
	if file == "" {
		return ""
	}
	return file + " line " + plainString(r["line"]) + " (" + plainString(r["side"]) + ")"
}

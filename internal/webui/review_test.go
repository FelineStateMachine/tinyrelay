package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

const reviewTestDiff = "diff --git a/README b/README\nindex 1111111..2222222 100644\n--- a/README\n+++ b/README\n@@ -1,3 +1,4 @@\n first\n-second\n+two\n+three\n fourth\ndiff --git a/docs/new.md b/docs/new.md\nnew file mode 100644\n--- /dev/null\n+++ b/docs/new.md\n@@ -0,0 +1,2 @@\n+# New\n+body\n\\ No newline at end of file\n"

func TestDiffLinesNumberBothSidesAndTrackFiles(t *testing.T) {
	lines, _ := previewLines(reviewTestDiff)
	rows := diffLines(lines)
	want := map[int]diffLine{
		3:  {Wrapper: "del", File: "README"},
		5:  {Wrapper: "b", File: "README"},
		6:  {File: "README", Old: 1, New: 1},
		7:  {Wrapper: "del", File: "README", Old: 2},
		8:  {Wrapper: "ins", File: "README", New: 2},
		9:  {Wrapper: "ins", File: "README", New: 3},
		10: {File: "README", Old: 3, New: 4},
		14: {Wrapper: "ins", File: "docs/new.md"},
		16: {Wrapper: "ins", File: "docs/new.md", New: 1},
		17: {Wrapper: "ins", File: "docs/new.md", New: 2},
		18: {File: "docs/new.md"},
	}
	for number, expected := range want {
		got := rows[number-1]
		expected.Number, expected.Text = number, got.Text
		if got != expected {
			t.Fatalf("row %d = %+v, want %+v", number, got, expected)
		}
	}
	if got := rows[11]; got.File != "" || got.Old != 0 || got.New != 0 {
		t.Fatalf("a new diff header kept the previous file: %+v", got)
	}
}

type reviewBackend struct{ *fakeBackend }

func (b *reviewBackend) Query(ctx context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	b.call = method
	b.params = params
	owner := b.policy.Owner
	root := map[string]any{"id": strings.Repeat("b", 64), "title": "Rename second", "kind": 1618, "author": owner, "content": "Why", "status": "open"}
	reply := func(id, content string, extra ...[]string) map[string]any {
		tags := append([][]string{{"a", "30617:" + owner + ":test"}, {"E", strings.Repeat("b", 64), "", owner}, {"K", "1618"}, {"P", owner}}, extra...)
		return map[string]any{"id": id, "kind": 1111, "pubkey": strings.Repeat("c", 64), "created_at": 1700000000, "content": content, "tags": tags}
	}
	anchored := reply(strings.Repeat("1", 64), "Prefer a longer word.", []string{"file", "README"}, []string{"line", "2", "new"})
	anchored["file"], anchored["line"], anchored["side"] = "README", 2, "new"
	lost := reply(strings.Repeat("2", 64), "Outside the diff.", []string{"file", "missing.txt"}, []string{"line", "9", "old"})
	lost["file"], lost["line"], lost["side"] = "missing.txt", 9, "old"
	plain := reply(strings.Repeat("3", 64), "Looks good overall.")
	return map[string]any{"item": root, "replies": []any{plain, lost, anchored}, "can_status": true, "diff": reviewTestDiff, "repository": map[string]any{"owner": owner, "clone": []string{"https://git.example/test.git"}}}, nil
}

func TestPullRequestPagePlacesAnchoredCommentsUnderDiffLines(t *testing.T) {
	for _, viewer := range []string{"", strings.Repeat("a", 64)} {
		backend := &reviewBackend{&fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
		app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return viewer, nil }})
		if err != nil {
			t.Fatal(err)
		}
		query := url.Values{"owner": {backend.policy.Owner}, "repo": {"test"}}
		link := repoURL(query, "pr", strings.Repeat("b", 64))
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, link, nil))
		body := w.Body.String()
		if w.Code != http.StatusOK || backend.call != "browsepull" {
			t.Fatalf("viewer %q: %d %s", viewer, w.Code, body)
		}
		if strings.Contains(body, "class=") {
			t.Fatalf("viewer %q: markup carries class attributes", viewer)
		}
		// The anchored comment sits in the row after new line 2 of README,
		// which is rendered diff row 8; the plain and unplaced comments stay
		// in the thread, the unplaced one with its anchor shown.
		row := strings.Index(body, `<tr id="D8">`)
		nextRow := strings.Index(body, `<tr id="D9">`)
		if row < 0 || nextRow < row || !strings.Contains(body[row:nextRow], "Prefer a longer word.") || !strings.Contains(body[row:nextRow], `<nostr-event id="event-`+strings.Repeat("1", 64)+`">`) {
			t.Fatalf("viewer %q: anchored comment not placed under its line: %s", viewer, body[row:nextRow])
		}
		thread := body[strings.Index(body, `<ol id="events">`):]
		if strings.Contains(thread, "Prefer a longer word.") || !strings.Contains(thread, "Looks good overall.") || !strings.Contains(thread, "on missing.txt line 9 (old)") {
			t.Fatalf("viewer %q: thread = %s", viewer, thread)
		}
		commentLink := strings.Contains(body, `file=README&amp;line=2&amp;side=new#reply-compose">comment</a>`)
		removedLink := strings.Contains(body, `file=README&amp;line=2&amp;side=old#reply-compose">comment</a>`)
		if viewer == "" {
			if commentLink || strings.Contains(body, "<nostr-compose") {
				t.Fatal("guest received comment links or a compose")
			}
			continue
		}
		if !commentLink || !removedLink || !strings.Contains(body, `&amp;file=docs%2Fnew.md&amp;line=1&amp;side=new#reply-compose">comment</a>`) {
			t.Fatalf("comment links missing: %s", body)
		}
		if strings.Contains(body, `<input type="hidden" name="file"`) {
			t.Fatal("compose was preset without a query anchor")
		}
		w = httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, link+"&file=README&line=2&side=new", nil))
		body = w.Body.String()
		for _, want := range []string{`<nostr-compose id="reply-compose" kind="1111"`, `<input type="hidden" name="file" value="README">`, `<input type="hidden" name="line" value="2">`, `<input type="hidden" name="side" value="new">`, `<p id="reply-anchor">Commenting on <code>README</code> line 2 (new side).`} {
			if !strings.Contains(body, want) {
				t.Fatalf("preset compose missing %s: %s", want, body)
			}
		}
		w = httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, link+"&file=README&line=0&side=left", nil))
		if strings.Contains(w.Body.String(), `name="file"`) {
			t.Fatal("an invalid query anchor preset the compose")
		}
	}
}

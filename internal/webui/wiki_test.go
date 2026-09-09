package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

const (
	wikiOwner  = "1111111111111111111111111111111111111111111111111111111111111111"
	wikiOther  = "2222222222222222222222222222222222222222222222222222222222222222"
	wikiMerge  = "3333333333333333333333333333333333333333333333333333333333333333"
	wikiOwnerV = "4444444444444444444444444444444444444444444444444444444444444444"
	wikiOtherV = "5555555555555555555555555555555555555555555555555555555555555555"
	// wikiOwnerOld and wikiOtherOld are archived revisions that the
	// current versions replaced.
	wikiOwnerOld = "7777777777777777777777777777777777777777777777777777777777777777"
	wikiOtherOld = "8888888888888888888888888888888888888888888888888888888888888888"
)

// wikiBackend answers the three wiki queries with one page that has two
// versions and an open merge request from the second author to the owner.
type wikiBackend struct {
	fakeBackend
	calls   []string
	params  map[string]map[string]any
	private bool
	// proposal makes the second author's version a proposal in this state
	// (pending, approved or rejected), decided by the owner when it is not
	// pending, with an approved earlier revision behind it in the history.
	// approver marks the caller as allowed to decide, and shown makes the
	// proposal the page's shown version.
	proposal string
	approver bool
	shown    bool
	// archived gives the owner's version an earlier revision, which a
	// request naming it by version opens.
	archived bool
}

func (b *wikiBackend) Query(_ context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	b.calls = append(b.calls, method)
	var object map[string]any
	if len(params) == 1 {
		_ = json.Unmarshal(params[0], &object)
	}
	if b.params == nil {
		b.params = map[string]map[string]any{}
	}
	b.params[method] = object
	if b.private && actor == "" {
		return nil, errors.New("auth-required: sign in to read this relay")
	}
	owner := map[string]any{"id": wikiOwnerV, "author": wikiOwner, "created_at": float64(1788876000), "d": "release-notes-1-4", "title": "Release notes 1.4 <b>", "summary": "What changed", "coordinate": "30818:" + wikiOwner + ":release-notes-1-4", "likes": float64(0), "revision": float64(1), "revisions": float64(1),
		"content": "# Highlights\n\nPrivate tenants and _encrypted_ uploads. See [[Files and Private Repositories]].\n\n<script>alert(1)</script>", "links": []any{"files-and-private-repositories"}}
	other := map[string]any{"id": wikiOtherV, "author": wikiOther, "created_at": float64(1788880000), "d": "release-notes-1-4", "title": "Release notes 1.4", "coordinate": "30818:" + wikiOther + ":release-notes-1-4", "likes": float64(0), "revision": float64(1), "revisions": float64(1), "fork": map[string]any{"a": owner["coordinate"], "e": wikiOwnerV}, "content": "# Highlights\n\nA proposed rewrite."}
	merge := map[string]any{"id": wikiMerge, "author": wikiOther, "created_at": float64(1788881000), "content": "Please take my rewrite", "target": owner["coordinate"], "target_d": "release-notes-1-4", "destination": wikiOwner, "source": wikiOtherV, "status": "open"}
	shown, preferredBy := owner, "owner"
	history := []any{other, owner}
	if b.proposal != "" {
		other["proposal"], other["approval"], other["revision"], other["revisions"] = true, b.proposal, float64(2), float64(2)
		if b.proposal != "pending" {
			other["approval_event"], other["approval_at"], other["approval_by"] = wikiMerge, float64(1788882000), wikiOwner
		}
		old := map[string]any{"id": wikiOtherOld, "author": wikiOther, "created_at": float64(1788878000), "d": "release-notes-1-4", "title": "Release notes 1.4", "coordinate": other["coordinate"], "likes": float64(0), "revision": float64(1), "revisions": float64(2), "superseded_by": wikiOtherV, "proposal": true, "approval": "approved", "approval_event": wikiMerge, "approval_at": float64(1788879000), "approval_by": wikiOwner}
		history = []any{other, old, owner}
		if b.shown {
			shown = other
		}
	}
	if b.archived {
		owner["revision"], owner["revisions"] = float64(2), float64(2)
		old := map[string]any{"id": wikiOwnerOld, "author": wikiOwner, "created_at": float64(1788870000), "d": "release-notes-1-4", "title": "Release notes 1.3", "coordinate": owner["coordinate"], "likes": float64(1), "revision": float64(1), "revisions": float64(2), "superseded_by": wikiOwnerV, "content": "# Highlights\n\nThe first draft."}
		history = append(history, old)
		if object["version"] == wikiOwnerOld {
			shown, preferredBy = old, "version"
		}
	}
	switch method {
	case "browsewiki":
		return map[string]any{"items": []any{map[string]any{"d": "release-notes-1-4", "title": "Release notes 1.4 <b>", "version": shown, "versions": float64(2), "open_merges": float64(1)}}, "next_cursor": "release-notes-1-4", "can_approve": b.approver}, nil
	case "browsewikipage":
		if object["d"] != "release-notes-1-4" {
			return nil, errors.New("not found: wiki page")
		}
		return map[string]any{"d": "release-notes-1-4", "title": "Release notes 1.4 <b>", "version": shown, "preferred_by": preferredBy, "versions": []any{other, owner}, "history": history, "merges": []any{merge}, "redirects_to": []any{map[string]any{"d": "changelog-1-4", "target_d": "release-notes-1-4"}}, "redirects_from": []any{}, "can_approve": b.approver}, nil
	case "browsewikimerge":
		return map[string]any{"merge": merge, "proposed": other, "target": owner}, nil
	}
	return nil, errors.New("unsupported: " + method)
}

func wikiApp(t *testing.T, b *wikiBackend, actor string) *App {
	t.Helper()
	app, err := New(b, Options{Actor: func(*http.Request) (string, error) { return actor, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func wikiGet(t *testing.T, app *App, path string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", path, recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if strings.Contains(body, "class=") {
		t.Fatalf("%s carries a class attribute", path)
	}
	if strings.Contains(body, "\u00b7") {
		t.Fatalf("%s carries a middle dot", path)
	}
	return body
}

func wantAll(t *testing.T, path, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("%s missing %q", path, want)
		}
	}
}

func wantNone(t *testing.T, path, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if strings.Contains(body, want) {
			t.Errorf("%s unexpectedly contains %q", path, want)
		}
	}
}

func TestWikiListRendersPagesAndSearch(t *testing.T) {
	b := &wikiBackend{fakeBackend: fakeBackend{policy: policy.Defaults(wikiOwner)}}
	app := wikiApp(t, b, wikiOwner)
	body := wikiGet(t, app, "/wiki?q=release")
	if b.calls[0] != "browsewiki" || b.params["browsewiki"]["q"] != "release" {
		t.Fatalf("calls=%v params=%v", b.calls, b.params)
	}
	wantAll(t, "/wiki", body,
		`href="/wiki" aria-current="page"`, `id="wiki-search"`, `value="release"`,
		`<a href="/wiki/release-notes-1-4">Release notes 1.4 &lt;b&gt;</a>`, `<nostr-name pubkey="`+wikiOwner+`"`,
		`<td>2</td><td>1 open</td>`, `2026-09-08 14:00 UTC`, `href="/wiki?cursor=release-notes-1-4&amp;q=release"`,
		`<b>demo</b> &raquo; <page-link url="http://relay.example/wiki`, `<a href="/wiki?new=1">new page</a>`)
	wantNone(t, "/wiki", body, "<wiki-compose")
	body = wikiGet(t, app, "/wiki?new=1")
	wantAll(t, "/wiki?new=1", body, `<h2>New page</h2>`, `<wiki-compose name="" author="" coordinate="" event="">`, `name="title"`, `name="name"`, `name="summary"`, `name="content"`, `value="publish"`, `<noscript>`)
	wantNone(t, "/wiki?new=1", body, "Propose to")
}

func TestWikiPageRendersArticleMergeBarAndPanel(t *testing.T) {
	b := &wikiBackend{fakeBackend: fakeBackend{policy: policy.Defaults(wikiOwner)}}
	path := "/wiki/Release%20Notes%201-4"
	body := wikiGet(t, wikiApp(t, b, wikiOwner), path)
	if b.params["browsewikipage"]["d"] != "release-notes-1-4" {
		t.Fatalf("page name was not normalized: %v", b.params["browsewikipage"])
	}
	wantAll(t, path, body,
		`<title>Release notes 1.4 &lt;b&gt; | wiki | demo</title>`, `<h1>Release notes 1.4 &lt;b&gt;</h1>`,
		`<merge-request><nostr-name pubkey="`+wikiOther+`"`, `proposes a new version of this page`, `href="/wiki/release-notes-1-4?merge=`+wikiMerge+`">compare</a>`,
		`<nostr-react event="`+wikiMerge+`" pubkey="`+wikiOther+`" kind="818"><button name="reaction" value="+">Accept</button><button name="reaction" value="-">Reject</button></nostr-react>`,
		`<a href="/e/`+wikiMerge+`">Reply</a>`,
		`<wiki-article><header>wiki | release-notes-1-4 | revision 1 by <nostr-name pubkey="`+wikiOwner+`"`, `version 1 of 2 | 1 fork</header>`,
		`<h1>Highlights</h1>`, `<em>encrypted</em>`, `<a href="/wiki/files-and-private-repositories">Files and Private Repositories</a>`, `&lt;script&gt;alert(1)&lt;/script&gt;`,
		`<h4>Page</h4>`, `<th>versions</th><td>2</td>`, `<th>merge requests</th><td>1 open</td>`, `<th>links</th><td>1</td>`,
		`<h4>Other versions</h4>`, `href="/wiki/release-notes-1-4?version=`+wikiOtherV+`">222222222222</a> | fork`,
		`<h4>Also known as</h4>`, `<code>changelog-1-4</code>`,
		`href="/wiki/release-notes-1-4?edit=1">edit</a>`, `href="/wiki/release-notes-1-4?history=1">history</a>`,
		`<b>demo</b> &raquo; <page-link url="http://relay.example/wiki/release-notes-1-4" title="Copy the page address">wiki/release-notes-1-4</page-link>`)
	wantNone(t, path, body, "<script>alert", ">fork</a>")
	// The other author sees the request but cannot answer it, and forks
	// instead of editing.
	body = wikiGet(t, wikiApp(t, b, wikiOther), path)
	wantAll(t, path, body, `<merge-request>`, `href="/wiki/release-notes-1-4?edit=1">fork</a>`)
	wantNone(t, path, body, "<nostr-react", ">edit</a>")
}

func TestWikiPageCompareEditorAndHistory(t *testing.T) {
	b := &wikiBackend{fakeBackend: fakeBackend{policy: policy.Defaults(wikiOwner)}}
	path := "/wiki/release-notes-1-4?merge=" + wikiMerge
	body := wikiGet(t, wikiApp(t, b, wikiOwner), path)
	if b.params["browsewikimerge"]["id"] != wikiMerge {
		t.Fatalf("merge lookup params=%v", b.params["browsewikimerge"])
	}
	wantAll(t, path, body, `<div id="wiki-compare">`, `| open | Please take my rewrite</p>`, `<h2>Proposed version</h2>`, `A proposed rewrite.`, `<h2>Current version</h2>`, `<em>encrypted</em>`, `href="/wiki/release-notes-1-4">back to the page</a>`)

	path = "/wiki/release-notes-1-4?edit=1"
	body = wikiGet(t, wikiApp(t, b, wikiOther), path)
	wantAll(t, path, body, `<wiki-compose name="release-notes-1-4" author="`+wikiOwner+`" coordinate="30818:`+wikiOwner+`:release-notes-1-4" event="`+wikiOwnerV+`">`,
		`value="Release notes 1.4 &lt;b&gt;"`, `value="release-notes-1-4"`, `value="What changed"`, `&lt;script&gt;alert(1)&lt;/script&gt;</textarea>`,
		`<button name="action" value="propose">Propose to 111111111111</button>`, `JavaScript and a connected signer are required to publish.`)
	wantNone(t, path, body, "<wiki-article>")
	body = wikiGet(t, wikiApp(t, b, wikiOwner), path)
	wantNone(t, path, body, "Propose to")

	path = "/wiki/release-notes-1-4?history=1"
	body = wikiGet(t, wikiApp(t, b, wikiOwner), path)
	wantAll(t, path, body, `<th>author</th><th>revision</th><th>version</th>`, `<td>1 <small>current</small></td>`, `fork of 444444444444`, `href="/wiki/release-notes-1-4?version=`+wikiOwnerV+`">444444444444</a>`)
	wantAll(t, path, body, `<th>revisions</th><td>2</td>`)
	wantNone(t, path, body, "<small>replaced</small>")
}

func TestWikiHistoryListsRevisionsWithStates(t *testing.T) {
	b := &wikiBackend{fakeBackend: fakeBackend{policy: policy.Defaults(wikiOwner)}, proposal: "pending", approver: true, shown: true, archived: true}
	path := "/wiki/release-notes-1-4?history=1"
	body := wikiGet(t, wikiApp(t, b, wikiOwner), path)
	wantAll(t, path, body,
		`<tr data-state="pending"><td><nostr-name pubkey="`+wikiOther+`"`, `<td>2 <small>current</small></td><td><a href="/wiki/release-notes-1-4?version=`+wikiOtherV+`">555555555555</a></td><td>fork of 444444444444 | proposed pending</td>`,
		`<tr data-state="approved"><td><nostr-name pubkey="`+wikiOther+`"`, `<td>1 <small>replaced</small></td><td><a href="/wiki/release-notes-1-4?version=`+wikiOtherOld+`">888888888888</a></td><td> | proposed approved</td>`,
		`<td>2 <small>current</small></td><td><a href="/wiki/release-notes-1-4?version=`+wikiOwnerV+`">444444444444</a></td>`,
		`<td>1 <small>replaced</small></td><td><a href="/wiki/release-notes-1-4?version=`+wikiOwnerOld+`">777777777777</a></td><td> | 1 likes</td>`,
		`<th>revisions</th><td>4</td>`)
	// A member sees the rows without their proposal states.
	b.approver = false
	body = wikiGet(t, wikiApp(t, b, wikiMember), path)
	wantAll(t, path, body, `<td>1 <small>replaced</small></td><td><a href="/wiki/release-notes-1-4?version=`+wikiOtherOld+`">888888888888</a></td><td></td>`)
	wantNone(t, path, body, "<tr data-state=", "proposed ")
}

func TestWikiArchivedRevisionShowsANote(t *testing.T) {
	b := &wikiBackend{fakeBackend: fakeBackend{policy: policy.Defaults(wikiOwner)}, archived: true}
	path := "/wiki/release-notes-1-4?version=" + wikiOwnerOld
	body := wikiGet(t, wikiApp(t, b, wikiOther), path)
	if b.params["browsewikipage"]["version"] != wikiOwnerOld {
		t.Fatalf("version lookup params=%v", b.params["browsewikipage"])
	}
	wantAll(t, path, body,
		`<wiki-article><header>wiki | release-notes-1-4 | revision 1 by <nostr-name pubkey="`+wikiOwner+`"`,
		`<p id="wiki-revision-note">revision 1 of 2 by this author; a newer revision exists | <a href="/wiki/release-notes-1-4?author=`+wikiOwner+`">show the newest</a></p>`,
		`The first draft.`)
	// The author's current revision carries no note.
	path = "/wiki/release-notes-1-4"
	body = wikiGet(t, wikiApp(t, b, wikiOther), path)
	wantAll(t, path, body, `revision 2 by <nostr-name pubkey="`+wikiOwner+`"`, `<em>encrypted</em>`)
	wantNone(t, path, body, `id="wiki-revision-note"`, "The first draft.")
}

func TestWikiMissingPageOffersTheEditorToMembers(t *testing.T) {
	b := &wikiBackend{fakeBackend: fakeBackend{policy: policy.Defaults(wikiOwner)}}
	path := "/wiki/Whats%20Up%3F"
	body := wikiGet(t, wikiApp(t, b, wikiOwner), path)
	wantAll(t, path, body, `<h1>whats-up</h1>`, `There is no page named <code>whats-up</code> yet.`, `<wiki-compose name="whats-up"`, `<th>name</th><td><code>whats-up</code></td>`)
	wantNone(t, path, body, `role="alert"`)
	body = wikiGet(t, wikiApp(t, b, ""), path)
	wantAll(t, path, body, `<a href="/signin">Sign in</a> to write it.`)
	wantNone(t, path, body, "<wiki-compose")
}

func TestWikiGuestAccessFollowsTheReadsPolicy(t *testing.T) {
	members := policy.Defaults(wikiOwner)
	members.Reads = "members"
	b := &wikiBackend{fakeBackend: fakeBackend{policy: members}, private: true}
	for _, path := range []string{"/wiki", "/wiki/release-notes-1-4"} {
		body := wikiGet(t, wikiApp(t, b, ""), path)
		wantAll(t, path, body, `<p role="alert">auth-required: sign in to read this relay</p>`, `browsing as guest`)
		wantNone(t, path, body, "<wiki-article>", "<merge-request>", "Release notes 1.4", "<wiki-compose")
		body = wikiGet(t, wikiApp(t, b, wikiOther), path)
		wantAll(t, path, body, "Release notes 1.4")
		wantNone(t, path, body, `role="alert"`)
	}
}

const wikiMember = "6666666666666666666666666666666666666666666666666666666666666666"

func TestWikiProposalBarOffersTheDecisionToApprovers(t *testing.T) {
	b := &wikiBackend{fakeBackend: fakeBackend{policy: policy.Defaults(wikiOwner)}, proposal: "pending", approver: true, shown: true}
	path := "/wiki/release-notes-1-4"
	body := wikiGet(t, wikiApp(t, b, wikiOwner), path)
	wantAll(t, path, body,
		`<wiki-proposal data-state="pending">Proposed by <nostr-name pubkey="`+wikiOther+`"`, `| pending<nostr-react event="`+wikiOtherV+`" pubkey="`+wikiOther+`" kind="30818"><button name="reaction" value="+">Accept</button><button name="reaction" value="-">Reject</button></nostr-react><small>showing pending revision 2; readers see approved revision 1</small></wiki-proposal>`,
		`<wiki-article><header>wiki | release-notes-1-4 | revision 2 by <nostr-name pubkey="`+wikiOther+`"`, `A proposed rewrite.`)
	// A decided proposal names the decision, its time and the deciding key,
	// and offers no buttons. Readers still see the earlier approved
	// revision while the edit is rejected, and the edit itself once it is
	// approved.
	for _, state := range []string{"approved", "rejected"} {
		b.proposal = state
		body = wikiGet(t, wikiApp(t, b, wikiOwner), path)
		wantAll(t, path, body, `<wiki-proposal data-state="`+state+`">Proposed by <nostr-name pubkey="`+wikiOther+`"`, `| `+state+` 2026-09-08 15:40 UTC by <nostr-name pubkey="`+wikiOwner+`"`)
		wantNone(t, path, body, "<nostr-react", ">Accept<")
		if state == "rejected" {
			wantAll(t, path, body, `<small>showing rejected revision 2; readers see approved revision 1</small></wiki-proposal>`)
		} else {
			wantNone(t, path, body, "readers see approved revision")
		}
	}
	// A pending proposal that is not the shown version is marked in the
	// panel and the history, with no bar above the article.
	b.proposal, b.shown = "pending", false
	body = wikiGet(t, wikiApp(t, b, wikiOwner), path)
	wantAll(t, path, body, `<h4>Other versions</h4><ul><li data-state="pending"><a href="/wiki/release-notes-1-4?version=`+wikiOtherV+`">222222222222</a> | fork | proposed pending <small>`)
	wantNone(t, path, body, "<wiki-proposal")
	body = wikiGet(t, wikiApp(t, b, wikiOwner), path+"?history=1")
	wantAll(t, path, body, `<tr data-state="pending"><td><nostr-name pubkey="`+wikiOther+`"`, `fork of 444444444444 | proposed pending</td>`)
}

func TestWikiProposalAuthorSeesTheStateWithoutButtons(t *testing.T) {
	b := &wikiBackend{fakeBackend: fakeBackend{policy: policy.Defaults(wikiOwner)}, proposal: "pending", shown: true}
	path := "/wiki/release-notes-1-4"
	body := wikiGet(t, wikiApp(t, b, wikiOther), path)
	wantAll(t, path, body, `<wiki-proposal data-state="pending">Proposed by <nostr-name pubkey="`+wikiOther+`"`, `| pending<small>showing pending revision 2; readers see approved revision 1</small></wiki-proposal>`)
	wantNone(t, path, body, "<nostr-react", ">Accept<", ">Reject<")
	body = wikiGet(t, wikiApp(t, b, wikiOther), "/wiki")
	wantAll(t, "/wiki", body, `<tr data-state="pending"><td><a href="/wiki/release-notes-1-4">Release notes 1.4 &lt;b&gt;</a> <small>proposed | pending</small></td>`)
}

func TestWikiProposalStateIsHiddenFromMembers(t *testing.T) {
	b := &wikiBackend{fakeBackend: fakeBackend{policy: policy.Defaults(wikiOwner)}, proposal: "approved", shown: true}
	for _, path := range []string{"/wiki/release-notes-1-4", "/wiki/release-notes-1-4?history=1", "/wiki"} {
		body := wikiGet(t, wikiApp(t, b, wikiMember), path)
		wantAll(t, path, body, "Release notes 1.4")
		wantNone(t, path, body, "<wiki-proposal", "<tr data-state=", "<li data-state=", "proposed | ", "| proposed ", "<nostr-react")
	}
}

func TestWikiListMarksProposalRowsForApprovers(t *testing.T) {
	b := &wikiBackend{fakeBackend: fakeBackend{policy: policy.Defaults(wikiOwner)}, proposal: "rejected", approver: true, shown: true}
	body := wikiGet(t, wikiApp(t, b, wikiOwner), "/wiki")
	wantAll(t, "/wiki", body, `<tr data-state="rejected"><td><a href="/wiki/release-notes-1-4">Release notes 1.4 &lt;b&gt;</a> <small>proposed | rejected</small></td><td><nostr-name pubkey="`+wikiOther+`"`)
	b.proposal = ""
	body = wikiGet(t, wikiApp(t, b, wikiOwner), "/wiki")
	wantNone(t, "/wiki", body, "<tr data-state=", "proposed | ")
}

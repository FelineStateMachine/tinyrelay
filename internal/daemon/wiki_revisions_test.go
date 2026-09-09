package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
)

// wikiImport signs an article and takes it in through the import path.
func wikiImport(t *testing.T, tenant *Tenant, secret, title string, createdAt int64) event.Event {
	t.Helper()
	d := strings.ToLower(strings.ReplaceAll(title, " ", "-"))
	e := event.Event{Kind: kindWikiArticle, CreatedAt: createdAt, Content: "Imported " + title, Tags: [][]string{{"d", d}, {"title", title}}}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	if err := tenant.ingest(context.Background(), e, replication.OriginImport); err != nil {
		t.Fatalf("import: %v", err)
	}
	return e
}

func wikiIDs(items []map[string]any) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item["id"].(string))
	}
	return ids
}

func TestWikiRevisionsAreKeptWhenAVersionIsReplaced(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	member := wikiKey(t, wikiMemberSecret)
	wikiMember(t, tenant, member)
	now := time.Now().Unix()
	first := wikiArticle(t, tenant, wikiOwnerSecret, "Bitcoin", now-500)
	second := wikiArticle(t, tenant, wikiOwnerSecret, "Bitcoin", now-400)
	third := wikiImport(t, tenant, wikiOwnerSecret, "Bitcoin", now-300)
	// A reaction to an old revision id is an ordinary event.
	wikiPublish(t, tenant, wikiMemberSecret, kindReaction, now-250, "+", []string{"e", first.ID})

	rows, err := tenant.store.WikiRevisions(ctx, owner, "bitcoin")
	if err != nil || len(rows) != 2 || rows[0].ID != second.ID || rows[0].SupersededBy != third.ID || rows[1].ID != first.ID || rows[1].SupersededBy != second.ID || rows[1].Event.Content != first.Content {
		t.Fatalf("stored revisions %+v %v", rows, err)
	}
	page := wikiCall(t, tenant, "", "browsewikipage", map[string]any{"d": "bitcoin"})
	versions := wikiItems(page, "versions")
	version := page["version"].(map[string]any)
	if len(versions) != 1 || version["id"] != third.ID || version["revision"].(float64) != 3 || version["revisions"].(float64) != 3 || version["superseded_by"] != nil || version["content"] != "Imported Bitcoin" {
		t.Fatalf("current version %v", page)
	}
	history := wikiItems(page, "history")
	if got := wikiIDs(history); strings.Join(got, ",") != third.ID+","+second.ID+","+first.ID {
		t.Fatalf("history order %v", got)
	}
	for i, want := range []struct {
		revision   float64
		superseded any
		likes      float64
	}{{3, nil, 0}, {2, third.ID, 0}, {1, second.ID, 1}} {
		h := history[i]
		if h["revision"].(float64) != want.revision || h["revisions"].(float64) != 3 || h["superseded_by"] != want.superseded || h["author"] != owner || h["title"] != "Bitcoin" || h["likes"].(float64) != want.likes {
			t.Fatalf("history entry %d %v", i, h)
		}
		if _, ok := h["content"]; ok {
			t.Fatalf("history entry carries content %v", h)
		}
	}

	// An archived revision opens by id with a note of what replaced it.
	page = wikiCall(t, tenant, "", "browsewikipage", map[string]any{"d": "bitcoin", "version": first.ID})
	version = page["version"].(map[string]any)
	if version["id"] != first.ID || version["content"] != first.Content || version["superseded_by"] != second.ID || version["revision"].(float64) != 1 || version["likes"].(float64) != 1 || page["preferred_by"] != "version" {
		t.Fatalf("archived version %v", version)
	}
	if len(wikiItems(page, "versions")) != 1 {
		t.Fatalf("archived lookup changed the versions %v", page["versions"])
	}
	items := wikiItems(wikiCall(t, tenant, "", "browsewiki", nil), "items")
	if len(items) != 1 || items[0]["version"].(map[string]any)["revision"].(float64) != 3 || items[0]["versions"].(float64) != 1 {
		t.Fatalf("list %v", items)
	}

	// A member's version of the same page is numbered on its own.
	memberVersion := wikiArticle(t, tenant, wikiMemberSecret, "Bitcoin", now-200)
	page = wikiCall(t, tenant, "", "browsewikipage", map[string]any{"d": "bitcoin", "author": member})
	if v := page["version"].(map[string]any); v["id"] != memberVersion.ID || v["revision"].(float64) != 1 || v["revisions"].(float64) != 1 {
		t.Fatalf("member version %v", v)
	}
	if history := wikiItems(page, "history"); len(history) != 4 || history[0]["id"] != memberVersion.ID {
		t.Fatalf("history across authors %v", wikiIDs(history))
	}
}

func TestWikiRevisionIsDroppedByTheAuthorsDeletion(t *testing.T) {
	_, tenant := testTenant(t)
	now := time.Now().Unix()
	first := wikiArticle(t, tenant, wikiOwnerSecret, "Bitcoin", now-500)
	second := wikiArticle(t, tenant, wikiOwnerSecret, "Bitcoin", now-400)
	wikiArticle(t, tenant, wikiOwnerSecret, "Bitcoin", now-300)
	// Someone else's deletion changes nothing.
	wikiPublish(t, tenant, wikiMemberSecret, 5, now-250, "", []string{"e", first.ID})
	if history := wikiItems(wikiCall(t, tenant, "", "browsewikipage", map[string]any{"d": "bitcoin"}), "history"); len(history) != 3 {
		t.Fatalf("foreign deletion dropped a revision %v", wikiIDs(history))
	}
	wikiPublish(t, tenant, wikiOwnerSecret, 5, now-200, "", []string{"e", first.ID})
	page := wikiCall(t, tenant, "", "browsewikipage", map[string]any{"d": "bitcoin"})
	history := wikiItems(page, "history")
	if len(history) != 2 || history[1]["id"] != second.ID || history[1]["revision"].(float64) != 1 || history[0]["revisions"].(float64) != 2 {
		t.Fatalf("history after deletion %v", history)
	}
	if _, err := tenant.Execute(context.Background(), "", "browsewikipage", []json.RawMessage{json.RawMessage(`{"d":"bitcoin","version":"` + first.ID + `"}`)}); err == nil || !strings.HasPrefix(err.Error(), "not found:") {
		t.Fatalf("deleted revision still opens: %v", err)
	}
	if rows, err := tenant.store.WikiRevisions(context.Background(), "", ""); err != nil || len(rows) != 1 {
		t.Fatalf("stored revisions %+v %v", rows, err)
	}
}

func TestWikiReadersFallBackToTheNewestApprovedRevision(t *testing.T) {
	app, tenant, owner, agent, moderator, member := wikiProposalTenant(t, "propose")
	ctx := context.Background()
	now := time.Now().Unix()
	first := wikiArticle(t, tenant, testAgentSecret, "Bitcoin", now-500)
	wikiPublish(t, tenant, wikiOwnerSecret, kindReaction, now-490, "+", []string{"e", first.ID})
	second := wikiArticle(t, tenant, testAgentSecret, "Bitcoin", now-400)
	// A page whose revisions were never approved stays absent.
	wikiArticle(t, tenant, testAgentSecret, "Nostr", now-380)
	wikiArticle(t, tenant, testAgentSecret, "Nostr", now-370)

	// The owner, a moderator and the agent see the pending edit in place,
	// with the approved revision behind it in the history.
	for _, actor := range []string{owner, moderator, agent} {
		page := wikiCall(t, tenant, actor, "browsewikipage", map[string]any{"d": "bitcoin"})
		version := page["version"].(map[string]any)
		if version["id"] != second.ID || version["approval"] != "pending" || version["revision"].(float64) != 2 || version["revisions"].(float64) != 2 || version["superseded_by"] != nil {
			t.Fatalf("%s page %v", actor, version)
		}
		history := wikiItems(page, "history")
		if len(history) != 2 || history[0]["id"] != second.ID || history[0]["approval"] != "pending" || history[1]["id"] != first.ID || history[1]["approval"] != "approved" || history[1]["superseded_by"] != second.ID {
			t.Fatalf("%s history %v", actor, history)
		}
		items := wikiItems(wikiCall(t, tenant, actor, "browsewiki", nil), "items")
		if len(items) != 2 || items[0]["version"].(map[string]any)["id"] != second.ID || items[0]["versions"].(float64) != 1 {
			t.Fatalf("%s list %v", actor, items)
		}
	}
	// A member and a guest read the approved revision while the edit
	// waits, and the history shows only what they may see.
	for _, actor := range []string{member, ""} {
		page := wikiCall(t, tenant, actor, "browsewikipage", map[string]any{"d": "bitcoin"})
		version := page["version"].(map[string]any)
		if version["id"] != first.ID || version["approval"] != "approved" || version["proposal"] != true || version["content"] != first.Content || version["superseded_by"] != second.ID || version["revision"].(float64) != 1 || version["revisions"].(float64) != 2 {
			t.Fatalf("%q page %v", actor, version)
		}
		if page["can_approve"] != false {
			t.Fatalf("%q page %v", actor, page)
		}
		versions := wikiItems(page, "versions")
		history := wikiItems(page, "history")
		if len(versions) != 1 || versions[0]["id"] != first.ID || len(history) != 1 || history[0]["id"] != first.ID {
			t.Fatalf("%q versions %v history %v", actor, wikiIDs(versions), wikiIDs(history))
		}
		items := wikiItems(wikiCall(t, tenant, actor, "browsewiki", nil), "items")
		if len(items) != 1 || items[0]["d"] != "bitcoin" || items[0]["version"].(map[string]any)["id"] != first.ID || items[0]["versions"].(float64) != 1 {
			t.Fatalf("%q list %v", actor, items)
		}
		for _, params := range []string{`{"d":"bitcoin","version":"` + second.ID + `"}`, `{"d":"nostr"}`} {
			if _, err := tenant.Execute(ctx, actor, "browsewikipage", []json.RawMessage{json.RawMessage(params)}); err == nil || !strings.HasPrefix(err.Error(), "not found:") {
				t.Fatalf("%q read %s: %v", actor, params, err)
			}
		}
	}

	// The MCP tools read through the same code. An empty secret signs as
	// the owner; the other key has no standing on the relay.
	call := func(name string, arguments map[string]any, secret string) map[string]any {
		t.Helper()
		w, response := mcpCall{method: "tools/call", name: name, arguments: arguments, sign: true, secret: secret}.do(t, app, "/mcp")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		result, isError := mcpToolResult(t, response)
		if isError {
			t.Fatalf("%s as %q: %v", name, secret, result)
		}
		value, _ := result["structuredContent"].(map[string]any)
		return value
	}
	for _, viewer := range []struct {
		secret string
		id     string
	}{{"", second.ID}, {testModSecret, second.ID}, {testAgentSecret, second.ID}, {wikiMemberSecret, first.ID}, {wikiOtherSecret, first.ID}} {
		page := call("read_wiki_page", map[string]any{"d": "bitcoin"}, viewer.secret)
		history, _ := page["history"].([]any)
		if page["version"].(map[string]any)["id"] != viewer.id || len(history) != map[bool]int{true: 2, false: 1}[viewer.id == second.ID] {
			t.Fatalf("read_wiki_page as %q: %v", viewer.secret, page)
		}
		items, _ := call("list_wiki", map[string]any{}, viewer.secret)["items"].([]any)
		if len(items) == 0 || items[0].(map[string]any)["version"].(map[string]any)["id"] != viewer.id {
			t.Fatalf("list_wiki as %q: %v", viewer.secret, items)
		}
	}

	// A rejected edit keeps the approved revision in view; approving the
	// edit shows it to everyone with the full history.
	wikiPublish(t, tenant, testModSecret, kindReaction, now-390, "-", []string{"e", second.ID})
	if page := wikiCall(t, tenant, member, "browsewikipage", map[string]any{"d": "bitcoin"}); page["version"].(map[string]any)["id"] != first.ID {
		t.Fatalf("rejected edit shown %v", page["version"])
	}
	wikiPublish(t, tenant, wikiOwnerSecret, kindReaction, now-380, "+", []string{"e", second.ID})
	for _, actor := range []string{owner, member, ""} {
		page := wikiCall(t, tenant, actor, "browsewikipage", map[string]any{"d": "bitcoin"})
		history := wikiItems(page, "history")
		if page["version"].(map[string]any)["id"] != second.ID || len(history) != 2 || history[1]["superseded_by"] != second.ID {
			t.Fatalf("%q after approval %v", actor, page)
		}
		// The approved older revision still opens from the history.
		page = wikiCall(t, tenant, actor, "browsewikipage", map[string]any{"d": "bitcoin", "version": first.ID})
		if v := page["version"].(map[string]any); v["id"] != first.ID || v["approval"] != "approved" || v["superseded_by"] != second.ID || v["content"] != first.Content {
			t.Fatalf("%q archived approved revision %v", actor, v)
		}
	}
}

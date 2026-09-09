package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// wikiProposalTenant grants the agent key wiki access and returns the
// owner, agent, moderator and member keys.
func wikiProposalTenant(t *testing.T, access string) (*App, *Tenant, string, string, string, string) {
	t.Helper()
	app, tenant := testTenant(t)
	owner := tenant.Policy().Owner
	agent, moderator, member := wikiKey(t, testAgentSecret), wikiKey(t, testModSecret), wikiKey(t, wikiMemberSecret)
	setRole(t, tenant, moderator, "moderator")
	wikiMember(t, tenant, member)
	now := time.Now().Unix()
	if err := publishAs(t, tenant, agentGrantEvent(t, agent, now-1000, now+3600, []string{"wiki", access})); err != nil {
		t.Fatalf("grant rejected: %v", err)
	}
	return app, tenant, owner, agent, moderator, member
}

func wikiVersionByID(t *testing.T, versions []map[string]any, id string) map[string]any {
	t.Helper()
	for _, v := range versions {
		if v["id"] == id {
			return v
		}
	}
	t.Fatalf("version %s missing from %v", id, versions)
	return nil
}

func TestWikiProposalStateFollowsOwnerAndModeratorReactions(t *testing.T) {
	_, tenant, owner, agent, moderator, _ := wikiProposalTenant(t, "propose")
	now := time.Now().Unix()

	first := wikiArticle(t, tenant, testAgentSecret, "Bitcoin", now-500)
	page := wikiCall(t, tenant, owner, "browsewikipage", map[string]any{"d": "bitcoin"})
	version := page["version"].(map[string]any)
	if version["id"] != first.ID || version["proposal"] != true || version["approval"] != "pending" || version["approval_event"] != nil || page["can_approve"] != true {
		t.Fatalf("new proposal %v", page)
	}

	// A member's + is a like, not an approval; the owner's + approves.
	wikiPublish(t, tenant, wikiMemberSecret, kindReaction, now-490, "+", []string{"e", first.ID})
	page = wikiCall(t, tenant, owner, "browsewikipage", map[string]any{"d": "bitcoin"})
	if page["version"].(map[string]any)["approval"] != "pending" {
		t.Fatalf("member reaction counted %v", page["version"])
	}
	approval := wikiPublish(t, tenant, wikiOwnerSecret, kindReaction, now-480, "+", []string{"e", first.ID}, []string{"p", agent}, []string{"k", "30818"})
	page = wikiCall(t, tenant, owner, "browsewikipage", map[string]any{"d": "bitcoin"})
	version = page["version"].(map[string]any)
	if version["approval"] != "approved" || version["approval_event"] != approval.ID || version["approval_by"] != owner || version["approval_at"].(float64) != float64(now-480) {
		t.Fatalf("approved proposal %v", version)
	}

	// An edit is a new event id and needs its own approval.
	second := wikiArticle(t, tenant, testAgentSecret, "Bitcoin", now-400)
	page = wikiCall(t, tenant, owner, "browsewikipage", map[string]any{"d": "bitcoin"})
	version = page["version"].(map[string]any)
	if version["id"] != second.ID || version["approval"] != "pending" || version["approval_event"] != nil {
		t.Fatalf("edit kept the approval %v", version)
	}
	items := wikiItems(wikiCall(t, tenant, owner, "browsewiki", nil), "items")
	if len(items) != 1 || items[0]["version"].(map[string]any)["approval"] != "pending" || items[0]["version"].(map[string]any)["proposal"] != true {
		t.Fatalf("list state %v", items)
	}

	// A moderator decides too; the newest deciding reaction wins.
	rejection := wikiPublish(t, tenant, testModSecret, kindReaction, now-390, "-", []string{"e", second.ID})
	page = wikiCall(t, tenant, moderator, "browsewikipage", map[string]any{"d": "bitcoin"})
	version = page["version"].(map[string]any)
	if version["approval"] != "rejected" || version["approval_event"] != rejection.ID || version["approval_by"] != moderator || page["can_approve"] != true {
		t.Fatalf("rejected proposal %v", page)
	}
	wikiPublish(t, tenant, wikiOwnerSecret, kindReaction, now-380, "+", []string{"e", second.ID})
	page = wikiCall(t, tenant, owner, "browsewikipage", map[string]any{"d": "bitcoin"})
	if page["version"].(map[string]any)["approval"] != "approved" {
		t.Fatalf("newest decision %v", page["version"])
	}
}

func TestWikiEditGrantAndPeopleAreNotProposals(t *testing.T) {
	_, tenant, owner, _, _, _ := wikiProposalTenant(t, "edit")
	now := time.Now().Unix()
	agentPage := wikiArticle(t, tenant, testAgentSecret, "Lightning", now-300)
	memberPage := wikiArticle(t, tenant, wikiMemberSecret, "Lightning", now-200)
	page := wikiCall(t, tenant, "", "browsewikipage", map[string]any{"d": "lightning"})
	versions := wikiItems(page, "versions")
	if len(versions) != 2 {
		t.Fatalf("versions %v", versions)
	}
	for _, id := range []string{agentPage.ID, memberPage.ID} {
		v := wikiVersionByID(t, versions, id)
		if v["proposal"] != nil || v["approval"] != nil {
			t.Fatalf("version marked as a proposal %v", v)
		}
	}
	if page["can_approve"] != false {
		t.Fatalf("guest may approve %v", page)
	}
	if page = wikiCall(t, tenant, owner, "browsewikipage", map[string]any{"d": "lightning"}); page["can_approve"] != true {
		t.Fatalf("owner may not approve %v", page)
	}
}

func TestWikiPendingProposalsAreInvisibleToMembersAndGuests(t *testing.T) {
	app, tenant, owner, agent, moderator, member := wikiProposalTenant(t, "propose")
	ctx := context.Background()
	now := time.Now().Unix()
	pending := wikiArticle(t, tenant, testAgentSecret, "Bitcoin", now-500)
	ownerNostr := wikiArticle(t, tenant, wikiOwnerSecret, "Nostr", now-400)
	agentNostr := wikiArticle(t, tenant, testAgentSecret, "Nostr", now-300)

	// The owner, a moderator and the author see the pending versions with
	// their state; a member and a guest do not.
	for _, actor := range []string{owner, moderator, agent} {
		items := wikiItems(wikiCall(t, tenant, actor, "browsewiki", nil), "items")
		if len(items) != 2 || items[0]["d"] != "bitcoin" || items[0]["version"].(map[string]any)["approval"] != "pending" || items[1]["versions"].(float64) != 2 {
			t.Fatalf("%s list %v", actor, items)
		}
		page := wikiCall(t, tenant, actor, "browsewikipage", map[string]any{"d": "bitcoin"})
		if page["version"].(map[string]any)["id"] != pending.ID || page["version"].(map[string]any)["approval"] != "pending" {
			t.Fatalf("%s page %v", actor, page)
		}
		page = wikiCall(t, tenant, actor, "browsewikipage", map[string]any{"d": "nostr"})
		if len(wikiItems(page, "versions")) != 2 || page["version"].(map[string]any)["id"] != ownerNostr.ID {
			t.Fatalf("%s nostr page %v", actor, page)
		}
		if page["can_approve"] != (actor != agent) {
			t.Fatalf("%s can_approve %v", actor, page["can_approve"])
		}
	}
	for _, actor := range []string{member, ""} {
		items := wikiItems(wikiCall(t, tenant, actor, "browsewiki", nil), "items")
		if len(items) != 1 || items[0]["d"] != "nostr" || items[0]["versions"].(float64) != 1 || items[0]["version"].(map[string]any)["proposal"] != nil {
			t.Fatalf("%q list %v", actor, items)
		}
		if _, err := tenant.Execute(ctx, actor, "browsewikipage", []json.RawMessage{json.RawMessage(`{"d":"bitcoin"}`)}); err == nil || !strings.HasPrefix(err.Error(), "not found:") {
			t.Fatalf("%q saw the pending page: %v", actor, err)
		}
		page := wikiCall(t, tenant, actor, "browsewikipage", map[string]any{"d": "nostr"})
		versions := wikiItems(page, "versions")
		if len(versions) != 1 || versions[0]["id"] != ownerNostr.ID {
			t.Fatalf("%q nostr versions %v", actor, versions)
		}
		if _, err := tenant.Execute(ctx, actor, "browsewikipage", []json.RawMessage{json.RawMessage(`{"d":"nostr","version":"` + agentNostr.ID + `"}`)}); err == nil {
			t.Fatalf("%q opened the pending version by id", actor)
		}
		if _, err := tenant.Execute(ctx, actor, "browsewikipage", []json.RawMessage{json.RawMessage(`{"d":"nostr","author":"` + agent + `"}`)}); err != nil {
			t.Fatal(err)
		} else if page = wikiCall(t, tenant, actor, "browsewikipage", map[string]any{"d": "nostr", "author": agent}); page["version"].(map[string]any)["id"] != ownerNostr.ID {
			t.Fatalf("%q author preference selected the pending version %v", actor, page)
		}
	}

	// A rejection keeps the version hidden; an approval shows it to all.
	wikiPublish(t, tenant, testModSecret, kindReaction, now-290, "-", []string{"e", pending.ID})
	wikiPublish(t, tenant, wikiOwnerSecret, kindReaction, now-280, "+", []string{"e", agentNostr.ID})
	for _, actor := range []string{member, ""} {
		items := wikiItems(wikiCall(t, tenant, actor, "browsewiki", nil), "items")
		if len(items) != 1 || items[0]["d"] != "nostr" || items[0]["versions"].(float64) != 2 {
			t.Fatalf("%q list after decisions %v", actor, items)
		}
		page := wikiCall(t, tenant, actor, "browsewikipage", map[string]any{"d": "nostr", "version": agentNostr.ID})
		if v := page["version"].(map[string]any); v["approval"] != "approved" || v["proposal"] != true || v["content"] == "" {
			t.Fatalf("%q approved version %v", actor, v)
		}
	}
	if page := wikiCall(t, tenant, owner, "browsewikipage", map[string]any{"d": "bitcoin"}); page["version"].(map[string]any)["approval"] != "rejected" {
		t.Fatalf("owner rejected page %v", page)
	}

	// The MCP read tools go through the same functions. MCP requests are
	// signed, so the outsider is a key with no standing on the relay; an
	// empty secret signs as the owner.
	call := func(name string, arguments map[string]any, secret string) (map[string]any, bool) {
		t.Helper()
		w, response := mcpCall{method: "tools/call", name: name, arguments: arguments, sign: true, secret: secret}.do(t, app, "/mcp")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		return mcpToolResult(t, response)
	}
	structured := func(result map[string]any) map[string]any {
		value, _ := result["structuredContent"].(map[string]any)
		return value
	}
	for _, viewer := range []struct {
		secret string
		sees   bool
	}{{"", true}, {testModSecret, true}, {testAgentSecret, true}, {wikiMemberSecret, false}, {wikiOtherSecret, false}} {
		result, isError := call("list_wiki", map[string]any{}, viewer.secret)
		items, _ := structured(result)["items"].([]any)
		if isError || len(items) != map[bool]int{true: 2, false: 1}[viewer.sees] {
			t.Fatalf("list_wiki as %q: %v", viewer.secret, result)
		}
		result, isError = call("read_wiki_page", map[string]any{"d": "bitcoin"}, viewer.secret)
		if isError == viewer.sees {
			t.Fatalf("read_wiki_page as %q: %v", viewer.secret, result)
		}
		if viewer.sees && structured(result)["version"].(map[string]any)["approval"] != "rejected" {
			t.Fatalf("read_wiki_page state as %q: %v", viewer.secret, result)
		}
	}
}

func TestWikiProposalWakesOwnerAndModeratorDevices(t *testing.T) {
	_, tenant, owner, agent, moderator, member := wikiProposalTenant(t, "propose")
	ctx := context.Background()
	now := time.Now().Unix()
	for _, pubkey := range []string{owner, moderator, member, agent} {
		if _, err := tenant.store.DB().ExecContext(ctx, "INSERT INTO web_push(endpoint,pubkey,subscription,created_at) VALUES(?,?,?,?)", "https://push.example/"+pubkey[:8], pubkey, "{}", now); err != nil {
			t.Fatal(err)
		}
	}
	// A person's version wakes nobody.
	wikiArticle(t, tenant, wikiMemberSecret, "Bitcoin", now-100)
	if got := countIntents(t, tenant, notificationPush); got != 0 {
		t.Fatalf("unexpected notifications %d", got)
	}
	wikiArticle(t, tenant, testAgentSecret, "Bitcoin Basics", now-90)
	if got := countIntents(t, tenant, notificationPush); got != 2 {
		t.Fatalf("notifications %d, want the owner and the moderator", got)
	}
	for _, recipient := range []string{owner, moderator} {
		var payload string
		if err := tenant.store.DB().QueryRowContext(ctx, "SELECT payload FROM work_intents WHERE kind=? AND target=?", notificationPush, recipient).Scan(&payload); err != nil {
			t.Fatalf("notification for %s: %v", recipient, err)
		}
		var notice pushPayload
		if err := json.Unmarshal([]byte(payload), &notice); err != nil {
			t.Fatal(err)
		}
		if notice.Kind != pushApprovals || notice.Text != "helper proposes wiki: Bitcoin Basics" || notice.URL != "http://relay.test/wiki/bitcoin-basics" {
			t.Fatalf("notice %+v", notice)
		}
	}
	// A decision wakes nobody.
	page := wikiCall(t, tenant, owner, "browsewikipage", map[string]any{"d": "bitcoin-basics"})
	wikiPublish(t, tenant, wikiOwnerSecret, kindReaction, now-80, "+", []string{"e", page["version"].(map[string]any)["id"].(string)}, []string{"p", agent})
	if got := countIntents(t, tenant, notificationPush); got != 2 {
		t.Fatalf("decision queued notifications: %d", got)
	}
}

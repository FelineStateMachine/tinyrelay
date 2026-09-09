package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// The maintainer agent holds a maintain grant; the human maintainer is
// named in the announcement's maintainers tag.
const (
	proposalBotSecret   = "0000000000000000000000000000000000000000000000000000000000000005"
	proposalHumanSecret = wikiOtherSecret
	proposalGuestSecret = wikiThirdSecret
	proposalRepo        = "proposals"
)

// proposalKeys are the people around one repository.
type proposalKeys struct {
	owner, agent, bot, human, moderator, member string
	coordinate                                  string
}

// proposalTenant announces a repository with one human maintainer, grants
// the agent key the given level on it and the bot key maintain, and adds
// a moderator and a member.
func proposalTenant(t *testing.T, level string) (*App, *Tenant, proposalKeys) {
	t.Helper()
	app, tenant := testTenant(t)
	p := tenant.Policy()
	p.Features.Grasp = true
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	k := proposalKeys{owner: tenant.Policy().Owner, agent: wikiKey(t, testAgentSecret), bot: wikiKey(t, proposalBotSecret), human: wikiKey(t, proposalHumanSecret), moderator: wikiKey(t, testModSecret), member: wikiKey(t, testMemberSecret)}
	k.coordinate = "30617:" + k.owner + ":" + proposalRepo
	setRole(t, tenant, k.moderator, "moderator")
	wikiMember(t, tenant, k.member)
	wikiMember(t, tenant, k.human)
	now := time.Now().Unix()
	wikiPublish(t, tenant, testOwnerSecret, event.KIND_REPO, now-2000, "", []string{"d", proposalRepo}, []string{"clone", "http://relay.test/" + k.owner + "/" + proposalRepo + ".git"}, []string{"relays", "ws://relay.test"}, []string{"maintainers", k.human})
	kinds := [][]string{{"k", "1621"}, {"k", "1618"}, {"k", "1617"}, {"k", "1111"}, {"k", "7"}, {"k", "1630"}, {"k", "1631"}}
	if err := publishAs(t, tenant, agentGrantEvent(t, k.agent, now-1000, now+3600, append([][]string{{"repo", k.owner + ":" + proposalRepo + ":" + level}}, kinds...)...)); err != nil {
		t.Fatalf("agent grant rejected: %v", err)
	}
	if err := publishAs(t, tenant, agentGrantEvent(t, k.bot, now-1000, now+3600, append([][]string{{"repo", k.owner + ":" + proposalRepo + ":maintain"}}, kinds...)...)); err != nil {
		t.Fatalf("bot grant rejected: %v", err)
	}
	return app, tenant, k
}

func proposalRoot(t *testing.T, tenant *Tenant, k proposalKeys, secret string, kind int, subject string, createdAt int64) event.Event {
	t.Helper()
	return wikiPublish(t, tenant, secret, kind, createdAt, "Body of "+subject, []string{"a", k.coordinate}, []string{"subject", subject})
}

func proposalComment(t *testing.T, tenant *Tenant, k proposalKeys, secret string, root event.Event, content string, createdAt int64) event.Event {
	t.Helper()
	kind := strconv.Itoa(root.Kind)
	return wikiPublish(t, tenant, secret, 1111, createdAt, content, []string{"A", k.coordinate}, []string{"E", root.ID, "", root.PubKey}, []string{"K", kind}, []string{"P", root.PubKey}, []string{"e", root.ID, "", root.PubKey}, []string{"k", kind}, []string{"p", root.PubKey})
}

func proposalReaction(t *testing.T, tenant *Tenant, secret string, target event.Event, content string, createdAt int64) event.Event {
	t.Helper()
	return wikiPublish(t, tenant, secret, kindReaction, createdAt, content, []string{"e", target.ID, "", target.PubKey}, []string{"p", target.PubKey}, []string{"k", strconv.Itoa(target.Kind)})
}

func proposalArgs(k proposalKeys, id string) map[string]any {
	args := map[string]any{"owner": k.owner, "repo": proposalRepo}
	if id != "" {
		args["event"] = id
	}
	return args
}

func proposalDetail(t *testing.T, tenant *Tenant, actor, method string, k proposalKeys, id string) map[string]any {
	t.Helper()
	return wikiCall(t, tenant, actor, method, proposalArgs(k, id))
}

func proposalItem(t *testing.T, tenant *Tenant, actor, method string, k proposalKeys, id string) map[string]any {
	t.Helper()
	item, _ := proposalDetail(t, tenant, actor, method, k, id)["item"].(map[string]any)
	if item == nil {
		t.Fatalf("%s %s: no item", method, id)
	}
	return item
}

func proposalNotFound(t *testing.T, tenant *Tenant, actor, method string, k proposalKeys, id string) {
	t.Helper()
	raw, _ := json.Marshal(proposalArgs(k, id))
	if _, err := tenant.Execute(context.Background(), actor, method, []json.RawMessage{raw}); err == nil || !strings.HasPrefix(err.Error(), "not found:") {
		t.Fatalf("%s as %q saw %s: %v", method, actor, id, err)
	}
}

func proposalIDs(items []map[string]any) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item["id"].(string))
	}
	return ids
}

func TestRepositoryProposalStateFollowsDecidingReactions(t *testing.T) {
	_, tenant, k := proposalTenant(t, "propose")
	now := time.Now().Unix()

	issue := proposalRoot(t, tenant, k, testAgentSecret, event.KIND_GIT_ISSUE, "Needs review", now-900)
	item := proposalItem(t, tenant, k.owner, "browseissue", k, issue.ID)
	if item["proposal"] != true || item["approval"] != "pending" || item["approval_event"] != nil {
		t.Fatalf("new proposal %v", item)
	}
	if detail := proposalDetail(t, tenant, k.owner, "browseissue", k, issue.ID); detail["can_approve"] != true {
		t.Fatalf("owner may not approve %v", detail)
	}
	if detail := proposalDetail(t, tenant, k.agent, "browseissue", k, issue.ID); detail["can_approve"] != false {
		t.Fatalf("author may approve %v", detail)
	}

	// A member's + is a like and the agent's own does not count; the
	// owner's + approves.
	proposalReaction(t, tenant, testMemberSecret, issue, "+", now-890)
	proposalReaction(t, tenant, testAgentSecret, issue, "+", now-889)
	if item = proposalItem(t, tenant, k.owner, "browseissue", k, issue.ID); item["approval"] != "pending" {
		t.Fatalf("member or author reaction counted %v", item)
	}
	approval := proposalReaction(t, tenant, testOwnerSecret, issue, "+", now-880)
	item = proposalItem(t, tenant, k.owner, "browseissue", k, issue.ID)
	if item["approval"] != "approved" || item["approval_event"] != approval.ID || item["approval_by"] != k.owner || item["approval_at"].(float64) != float64(now-880) {
		t.Fatalf("approved proposal %v", item)
	}

	// An agent with a maintain grant decides a comment; an empty reaction
	// approves like +.
	comment := proposalComment(t, tenant, k, testAgentSecret, issue, "One more thing", now-870)
	replies := wikiItems(proposalDetail(t, tenant, k.owner, "browseissue", k, issue.ID), "replies")
	if len(replies) != 1 || replies[0]["proposal"] != true || replies[0]["approval"] != "pending" {
		t.Fatalf("new comment %v", replies)
	}
	decision := proposalReaction(t, tenant, proposalBotSecret, comment, "", now-860)
	replies = wikiItems(proposalDetail(t, tenant, k.owner, "browseissue", k, issue.ID), "replies")
	if replies[0]["approval"] != "approved" || replies[0]["approval_by"] != k.bot || replies[0]["approval_event"] != decision.ID {
		t.Fatalf("maintainer agent decision %v", replies)
	}

	// The human maintainer rejects a pull request, a moderator's newer +
	// approves it, and the owner's newer - rejects it again.
	pull := proposalRoot(t, tenant, k, testAgentSecret, event.KIND_GIT_PR, "Add tests", now-800)
	rejection := proposalReaction(t, tenant, proposalHumanSecret, pull, "-", now-790)
	item = proposalItem(t, tenant, k.human, "browsepull", k, pull.ID)
	if item["approval"] != "rejected" || item["approval_event"] != rejection.ID || item["approval_by"] != k.human {
		t.Fatalf("maintainer rejection %v", item)
	}
	if detail := proposalDetail(t, tenant, k.human, "browsepull", k, pull.ID); detail["can_approve"] != true {
		t.Fatalf("maintainer may not approve %v", detail)
	}
	proposalReaction(t, tenant, testModSecret, pull, "+", now-780)
	if item = proposalItem(t, tenant, k.moderator, "browsepull", k, pull.ID); item["approval"] != "approved" || item["approval_by"] != k.moderator {
		t.Fatalf("moderator decision %v", item)
	}
	proposalReaction(t, tenant, testOwnerSecret, pull, "-", now-770)
	if item = proposalItem(t, tenant, k.owner, "browsepull", k, pull.ID); item["approval"] != "rejected" {
		t.Fatalf("newest decision %v", item)
	}

	// A resubmitted issue is a new event id and starts pending; the list
	// carries the state of each.
	again := proposalRoot(t, tenant, k, testAgentSecret, event.KIND_GIT_ISSUE, "Needs review", now-700)
	list := proposalDetail(t, tenant, k.owner, "browseissues", k, "")
	items := wikiItems(list, "items")
	if len(items) != 2 || items[0]["id"] != again.ID || items[0]["approval"] != "pending" || items[1]["id"] != issue.ID || items[1]["approval"] != "approved" || items[1]["approval_by"] != k.owner || list["can_approve"] != true {
		t.Fatalf("list state %v", list)
	}
}

func TestRepositoryReadGrantIsNeverAProposal(t *testing.T) {
	_, tenant, k := proposalTenant(t, "read")
	now := time.Now().Unix()
	issue := proposalRoot(t, tenant, k, testAgentSecret, event.KIND_GIT_ISSUE, "Plain issue", now-500)
	proposalComment(t, tenant, k, testAgentSecret, issue, "Plain comment", now-490)
	for _, actor := range []string{k.owner, k.member, ""} {
		items := wikiItems(proposalDetail(t, tenant, actor, "browseissues", k, ""), "items")
		if len(items) != 1 || items[0]["proposal"] != nil || items[0]["approval"] != nil {
			t.Fatalf("%q list %v", actor, items)
		}
		detail := proposalDetail(t, tenant, actor, "browseissue", k, issue.ID)
		replies := wikiItems(detail, "replies")
		if detail["item"].(map[string]any)["proposal"] != nil || len(replies) != 1 || replies[0]["proposal"] != nil {
			t.Fatalf("%q detail %v", actor, detail)
		}
		if detail["can_approve"] != (actor == k.owner) {
			t.Fatalf("%q can_approve %v", actor, detail["can_approve"])
		}
	}
}

func TestRepositoryPendingProposalsAreInvisibleToMembersAndGuests(t *testing.T) {
	app, tenant, k := proposalTenant(t, "propose")
	now := time.Now().Unix()
	pendingIssue := proposalRoot(t, tenant, k, testAgentSecret, event.KIND_GIT_ISSUE, "Agent issue", now-900)
	ownerIssue := proposalRoot(t, tenant, k, testOwnerSecret, event.KIND_GIT_ISSUE, "Owner issue", now-800)
	memberReply := proposalComment(t, tenant, k, testMemberSecret, ownerIssue, "From a member", now-790)
	pendingReply := proposalComment(t, tenant, k, testAgentSecret, ownerIssue, "From the agent", now-780)
	pendingPull := proposalRoot(t, tenant, k, testAgentSecret, event.KIND_GIT_PR, "Agent pull", now-700)
	ownerPull := proposalRoot(t, tenant, k, testOwnerSecret, event.KIND_GIT_PR, "Owner pull", now-600)
	pendingPatch := proposalRoot(t, tenant, k, testAgentSecret, event.KIND_GIT_PATCH, "Agent patch", now-500)

	activity := func(actor string) []string {
		t.Helper()
		result := wikiCall(t, tenant, actor, "browserepo", map[string]any{"owner": k.owner, "repo": proposalRepo, "view": "activity", "limit": 50})
		return proposalIDs(wikiItems(result, "events"))
	}

	// The deciders and the author see the pending events with their state.
	for _, actor := range []string{k.owner, k.human, k.bot, k.moderator, k.agent} {
		issues := wikiItems(proposalDetail(t, tenant, actor, "browseissues", k, ""), "items")
		if len(issues) != 2 || issues[1]["id"] != pendingIssue.ID || issues[1]["approval"] != "pending" || issues[0]["proposal"] != nil {
			t.Fatalf("%s issues %v", actor, issues)
		}
		if item := proposalItem(t, tenant, actor, "browseissue", k, pendingIssue.ID); item["approval"] != "pending" {
			t.Fatalf("%s pending issue %v", actor, item)
		}
		replies := wikiItems(proposalDetail(t, tenant, actor, "browseissue", k, ownerIssue.ID), "replies")
		if len(replies) != 2 || replies[0]["id"] != pendingReply.ID || replies[0]["approval"] != "pending" || replies[1]["id"] != memberReply.ID || replies[1]["proposal"] != nil {
			t.Fatalf("%s replies %v", actor, replies)
		}
		pulls := wikiItems(proposalDetail(t, tenant, actor, "browsepulls", k, ""), "items")
		if len(pulls) != 2 || pulls[1]["id"] != pendingPull.ID || pulls[1]["approval"] != "pending" {
			t.Fatalf("%s pulls %v", actor, pulls)
		}
		if item := proposalItem(t, tenant, actor, "browsepull", k, pendingPull.ID); item["approval"] != "pending" {
			t.Fatalf("%s pending pull %v", actor, item)
		}
		if ids := activity(actor); !contains(ids, pendingPatch.ID) || !contains(ids, pendingIssue.ID) || !contains(ids, pendingReply.ID) {
			t.Fatalf("%s activity %v", actor, ids)
		}
	}
	// A member and a guest see none of them.
	for _, actor := range []string{k.member, ""} {
		issues := wikiItems(proposalDetail(t, tenant, actor, "browseissues", k, ""), "items")
		if len(issues) != 1 || issues[0]["id"] != ownerIssue.ID || issues[0]["proposal"] != nil {
			t.Fatalf("%q issues %v", actor, issues)
		}
		proposalNotFound(t, tenant, actor, "browseissue", k, pendingIssue.ID)
		detail := proposalDetail(t, tenant, actor, "browseissue", k, ownerIssue.ID)
		if replies := wikiItems(detail, "replies"); len(replies) != 1 || replies[0]["id"] != memberReply.ID || detail["can_approve"] != false {
			t.Fatalf("%q replies %v", actor, detail)
		}
		pulls := wikiItems(proposalDetail(t, tenant, actor, "browsepulls", k, ""), "items")
		if len(pulls) != 1 || pulls[0]["id"] != ownerPull.ID {
			t.Fatalf("%q pulls %v", actor, pulls)
		}
		proposalNotFound(t, tenant, actor, "browsepull", k, pendingPull.ID)
		if ids := activity(actor); contains(ids, pendingPatch.ID) || contains(ids, pendingIssue.ID) || contains(ids, pendingReply.ID) || contains(ids, pendingPull.ID) || !contains(ids, ownerIssue.ID) || !contains(ids, memberReply.ID) {
			t.Fatalf("%q activity %v", actor, ids)
		}
	}

	// A rejection keeps an event hidden; an approval shows it to all.
	proposalReaction(t, tenant, testOwnerSecret, pendingIssue, "-", now-400)
	proposalReaction(t, tenant, proposalHumanSecret, pendingPull, "+", now-390)
	proposalReaction(t, tenant, testModSecret, pendingReply, "+", now-380)
	for _, actor := range []string{k.member, ""} {
		proposalNotFound(t, tenant, actor, "browseissue", k, pendingIssue.ID)
		if item := proposalItem(t, tenant, actor, "browsepull", k, pendingPull.ID); item["proposal"] != true || item["approval"] != "approved" || item["approval_by"] != k.human {
			t.Fatalf("%q approved pull %v", actor, item)
		}
		if pulls := wikiItems(proposalDetail(t, tenant, actor, "browsepulls", k, ""), "items"); len(pulls) != 2 {
			t.Fatalf("%q pulls after approval %v", actor, pulls)
		}
		if replies := wikiItems(proposalDetail(t, tenant, actor, "browseissue", k, ownerIssue.ID), "replies"); len(replies) != 2 || replies[0]["approval"] != "approved" {
			t.Fatalf("%q replies after approval %v", actor, replies)
		}
	}
	if item := proposalItem(t, tenant, k.owner, "browseissue", k, pendingIssue.ID); item["approval"] != "rejected" {
		t.Fatalf("owner rejected issue %v", item)
	}

	// The MCP tools read through the same functions. An empty secret signs
	// as the owner; the guest is a key with no standing on the relay.
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
	}{{"", true}, {proposalHumanSecret, true}, {proposalBotSecret, true}, {testModSecret, true}, {testAgentSecret, true}, {testMemberSecret, false}, {proposalGuestSecret, false}} {
		result, isError := call("list_issues", proposalArgs(k, ""), viewer.secret)
		items, _ := structured(result)["items"].([]any)
		if isError || len(items) != map[bool]int{true: 2, false: 1}[viewer.sees] || structured(result)["can_approve"] != (viewer.sees && viewer.secret != testAgentSecret) {
			t.Fatalf("list_issues as %q: %v", viewer.secret, result)
		}
		result, isError = call("read_issue", proposalArgs(k, pendingIssue.ID), viewer.secret)
		if isError == viewer.sees {
			t.Fatalf("read_issue as %q: %v", viewer.secret, result)
		}
		if viewer.sees && structured(result)["item"].(map[string]any)["approval"] != "rejected" {
			t.Fatalf("read_issue state as %q: %v", viewer.secret, result)
		}
		result, isError = call("list_pull_requests", proposalArgs(k, ""), viewer.secret)
		if items, _ = structured(result)["items"].([]any); isError || len(items) != 2 {
			t.Fatalf("list_pull_requests as %q: %v", viewer.secret, result)
		}
		result, isError = call("read_pull_request", proposalArgs(k, pendingPull.ID), viewer.secret)
		if isError || structured(result)["item"].(map[string]any)["approval"] != "approved" {
			t.Fatalf("read_pull_request as %q: %v", viewer.secret, result)
		}
	}
	// A new pull request from the agent is pending again and hidden from
	// the member and the guest over MCP too.
	later := proposalRoot(t, tenant, k, testAgentSecret, event.KIND_GIT_PR, "Agent pull again", now-300)
	for _, secret := range []string{testMemberSecret, proposalGuestSecret} {
		result, isError := call("list_pull_requests", proposalArgs(k, ""), secret)
		if items, _ := structured(result)["items"].([]any); isError || len(items) != 2 {
			t.Fatalf("list_pull_requests after resubmit as %q: %v", secret, result)
		}
		if _, isError := call("read_pull_request", proposalArgs(k, later.ID), secret); !isError {
			t.Fatalf("read_pull_request as %q opened the pending pull request", secret)
		}
	}
	if result, isError := call("read_pull_request", proposalArgs(k, later.ID), ""); isError || structured(result)["item"].(map[string]any)["approval"] != "pending" {
		t.Fatalf("owner read_pull_request: %v", result)
	}
}

func TestRepositoryStatusIgnoresPendingEvents(t *testing.T) {
	_, tenant, k := proposalTenant(t, "propose")
	now := time.Now().Unix()
	issue := proposalRoot(t, tenant, k, testAgentSecret, event.KIND_GIT_ISSUE, "Agent issue", now-500)
	// A status change from the proposer is stored here as if it had arrived
	// through synchronization; the grant refuses it at the gate.
	closed := signedCollaborationEvent(t, testAgentSecret, 1632, now-490, [][]string{{"a", k.coordinate}, {"e", issue.ID}}, "closed")
	saveCollaborationEvents(t, tenant, closed)
	if item := proposalItem(t, tenant, k.owner, "browseissue", k, issue.ID); item["status"] != "open" {
		t.Fatalf("proposer status counted %v", item)
	}
	if items := wikiItems(proposalDetail(t, tenant, k.owner, "browseissues", k, ""), "items"); items[0]["status"] != "open" {
		t.Fatalf("proposer status counted in the list %v", items)
	}
	if detail := proposalDetail(t, tenant, k.owner, "browseissue", k, issue.ID); len(wikiItems(detail, "statuses")) != 0 {
		t.Fatalf("proposer status listed %v", detail["statuses"])
	}
	// The owner's status counts.
	wikiPublish(t, tenant, testOwnerSecret, 1631, now-480, "resolved", []string{"a", k.coordinate}, []string{"e", issue.ID})
	if item := proposalItem(t, tenant, k.owner, "browseissue", k, issue.ID); item["status"] != "resolved" {
		t.Fatalf("owner status ignored %v", item)
	}
}

func TestRepositoryProposalWakesOwnerAndMaintainerDevices(t *testing.T) {
	_, tenant, k := proposalTenant(t, "propose")
	ctx := context.Background()
	now := time.Now().Unix()
	for _, pubkey := range []string{k.owner, k.human, k.bot, k.moderator, k.member, k.agent} {
		if _, err := tenant.store.DB().ExecContext(ctx, "INSERT INTO web_push(endpoint,pubkey,subscription,created_at) VALUES(?,?,?,?)", "https://push.example/"+pubkey[:8], pubkey, "{}", now); err != nil {
			t.Fatal(err)
		}
	}
	// A member's issue wakes the owner as a reply, not as a request.
	proposalRoot(t, tenant, k, testMemberSecret, event.KIND_GIT_ISSUE, "Member issue", now-100)
	if got := countIntents(t, tenant, notificationPush); got != 1 {
		t.Fatalf("member issue notifications %d", got)
	}
	issue := proposalRoot(t, tenant, k, testAgentSecret, event.KIND_GIT_ISSUE, "Needs review", now-90)
	if got := countIntents(t, tenant, notificationPush); got != 4 {
		t.Fatalf("notifications %d, want the owner and the two maintainers", got-1)
	}
	for _, recipient := range []string{k.human, k.bot} {
		var payload string
		if err := tenant.store.DB().QueryRowContext(ctx, "SELECT payload FROM work_intents WHERE kind=? AND target=?", notificationPush, recipient).Scan(&payload); err != nil {
			t.Fatalf("notification for %s: %v", recipient, err)
		}
		var notice pushPayload
		if err := json.Unmarshal([]byte(payload), &notice); err != nil {
			t.Fatal(err)
		}
		if notice.Kind != pushApprovals || notice.Text != "helper proposes issue: Needs review" || notice.URL != "http://relay.test/repo?owner="+k.owner+"&repo="+proposalRepo+"&view=issue&id="+issue.ID {
			t.Fatalf("notice %+v", notice)
		}
	}
	// A decision wakes nobody.
	proposalReaction(t, tenant, testOwnerSecret, issue, "+", now-80)
	if got := countIntents(t, tenant, notificationPush); got != 4 {
		t.Fatalf("decision queued notifications: %d", got)
	}
	// A comment and a pull request name their kind and page; the notices
	// are read directly since the approvals category coalesces.
	comment := proposalComment(t, tenant, k, testAgentSecret, issue, "Also this line looks wrong", now-70)
	notices, ok := tenant.collaborationProposalNotices(ctx, comment)
	if !ok || len(notices) != 3 || notices[0].recipient != k.owner || notices[0].category != pushApprovals || notices[0].body != "helper proposes comment: Also this line looks wrong" || notices[0].url != "http://relay.test/repo?owner="+k.owner+"&repo="+proposalRepo+"&view=issue&id="+issue.ID {
		t.Fatalf("comment notices %v %+v", ok, notices)
	}
	pull := proposalRoot(t, tenant, k, testAgentSecret, event.KIND_GIT_PR, "Add tests", now-60)
	if notices, ok = tenant.collaborationProposalNotices(ctx, pull); !ok || notices[0].body != "helper proposes pull request: Add tests" || notices[0].url != "http://relay.test/repo?owner="+k.owner+"&repo="+proposalRepo+"&view=pr&id="+pull.ID {
		t.Fatalf("pull notices %v %+v", ok, notices)
	}
	if _, ok = tenant.collaborationProposalNotices(ctx, proposalRoot(t, tenant, k, testOwnerSecret, event.KIND_GIT_PR, "Owner pull", now-50)); ok {
		t.Fatal("an owner's pull request is a proposal")
	}
}

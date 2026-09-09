package daemon

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

const (
	approvalAskerSecret = "5555555555555555555555555555555555555555555555555555555555555555"
	approvalOtherSecret = "6666666666666666666666666666666666666666666666666666666666666666"
)

func approvalCall(t *testing.T, tenant *Tenant, actor, method string, params map[string]any) (map[string]any, error) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	value, err := tenant.Execute(context.Background(), actor, method, []json.RawMessage{raw})
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result, nil
}

func approvalByID(items []map[string]any, id string) map[string]any {
	for _, item := range items {
		if item["id"] == id {
			return item
		}
	}
	return nil
}

func TestBrowseApprovalsStatesAndAccess(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	asker, other := wikiKey(t, approvalAskerSecret), wikiKey(t, approvalOtherSecret)
	now := time.Now().Unix()

	open := wikiPublish(t, tenant, approvalAskerSecret, kindComment, now-400, "Publish release notes 1.4 as drafted?", []string{"request", "approve"}, []string{"p", owner}, []string{"subject", "Publish release notes 1.4"}, []string{"a", "30617:" + owner + ":notes"}, []string{"expiration", strconv.FormatInt(now+3600, 10)})
	decided := wikiPublish(t, tenant, approvalAskerSecret, kindThreadRoot, now-300, "Mark pull request 23 merged?", []string{"request", "decide"}, []string{"p", owner}, []string{"h", tenant.meta.Name})
	// An older denial, a newer approval and a stranger's reaction: the
	// newest reaction from an asked key wins and the stranger is ignored.
	wikiPublish(t, tenant, wikiOwnerSecret, kindReaction, now-250, "-", []string{"e", decided.ID}, []string{"p", asker})
	approval := wikiPublish(t, tenant, wikiOwnerSecret, kindReaction, now-200, "+", []string{"e", decided.ID}, []string{"p", asker})
	wikiPublish(t, tenant, approvalOtherSecret, kindReaction, now-100, "-", []string{"e", decided.ID}, []string{"p", asker})
	question := wikiPublish(t, tenant, approvalAskerSecret, kindComment, now-200, "Should the edge be mentioned?", []string{"request", "question"}, []string{"p", owner}, []string{"E", decided.ID, "", asker}, []string{"K", "11"}, []string{"P", asker})
	reply := wikiPublish(t, tenant, wikiOwnerSecret, kindComment, now-150, "Leave it out.", []string{"E", question.ID, "", asker}, []string{"K", "1111"}, []string{"P", asker}, []string{"e", question.ID, "", asker}, []string{"k", "1111"}, []string{"p", asker})
	// An expired request is saved directly, as it would have been before it
	// lapsed, and shows as expired until maintenance removes it.
	expired := event.Event{Kind: kindComment, CreatedAt: now - 7200, Content: "Rotate the key tonight?", Tags: [][]string{{"request", "approve"}, {"p", owner}, {"expiration", strconv.FormatInt(now-3600, 10)}}}
	if err := event.Sign(&expired, approvalAskerSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.Save(ctx, expired, storageSave(tenant)); err != nil {
		t.Fatal(err)
	}
	elsewhere := wikiPublish(t, tenant, approvalAskerSecret, kindComment, now-50, "Only for someone else", []string{"request", "approve"}, []string{"p", other})
	// Ordinary comments and unknown request types are not requests.
	wikiPublish(t, tenant, approvalAskerSecret, kindComment, now-40, "Plain mention", []string{"p", owner})
	wikiPublish(t, tenant, approvalAskerSecret, kindComment, now-30, "Unknown type", []string{"request", "veto"}, []string{"p", owner})

	result, err := approvalCall(t, tenant, owner, "browseapprovals", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	items := wikiItems(result, "items")
	if len(items) != 4 || approvalByID(items, elsewhere.ID) != nil {
		t.Fatalf("owner sees %d items: %v", len(items), items)
	}
	counts := result["counts"].(map[string]any)
	if counts["open"] != 1.0 || counts["answered"] != 2.0 || counts["expired"] != 1.0 {
		t.Fatalf("counts %v", counts)
	}
	if result["oldest_open"] != float64(open.CreatedAt) {
		t.Fatalf("oldest open %v", result["oldest_open"])
	}
	item := approvalByID(items, open.ID)
	if item["state"] != "open" || item["type"] != "approve" || item["subject"] != "Publish release notes 1.4" || item["asker"] != asker || item["expires"] != float64(now+3600) || item["answer"] != nil {
		t.Fatalf("open item %v", item)
	}
	if about := item["about"].(map[string]any); about["coordinate"] != "30617:"+owner+":notes" || !strings.HasSuffix(about["url"].(string), "/repo?owner="+owner+"&repo=notes&view=home") {
		t.Fatalf("about %v", about)
	}
	item = approvalByID(items, decided.ID)
	answer, _ := item["answer"].(map[string]any)
	if item["state"] != "answered" || item["room"] != tenant.meta.Name || answer == nil || answer["decision"] != "approved" || answer["id"] != approval.ID {
		t.Fatalf("decided item %v", item)
	}
	item = approvalByID(items, question.ID)
	answer, _ = item["answer"].(map[string]any)
	if item["state"] != "answered" || answer == nil || answer["decision"] != "replied" || answer["id"] != reply.ID || answer["content"] != "Leave it out." {
		t.Fatalf("question item %v", item)
	}
	if about := item["about"].(map[string]any); about["event"] != decided.ID || about["kind"] != "11" || !strings.HasSuffix(about["url"].(string), "/e/"+decided.ID) {
		t.Fatalf("question about %v", about)
	}
	if item = approvalByID(items, expired.ID); item["state"] != "expired" {
		t.Fatalf("expired item %v", item)
	}

	filtered, err := approvalCall(t, tenant, owner, "browseapprovals", map[string]any{"state": "open"})
	if err != nil {
		t.Fatal(err)
	}
	if items := wikiItems(filtered, "items"); len(items) != 1 || items[0]["id"] != open.ID {
		t.Fatalf("open filter %v", items)
	}
	if _, err := approvalCall(t, tenant, owner, "browseapprovals", map[string]any{"state": "bogus"}); err == nil || !strings.HasPrefix(err.Error(), "invalid:") {
		t.Fatalf("bogus state %v", err)
	}
	paged, err := approvalCall(t, tenant, owner, "browseapprovals", map[string]any{"limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	if items := wikiItems(paged, "items"); len(items) != 2 || paged["next_cursor"] == "" {
		t.Fatalf("paged %v", paged)
	}
	rest, err := approvalCall(t, tenant, owner, "browseapprovals", map[string]any{"limit": 2, "cursor": paged["next_cursor"]})
	if err != nil {
		t.Fatal(err)
	}
	if items := wikiItems(rest, "items"); len(items) != 2 || items[0]["id"] == wikiItems(paged, "items")[0]["id"] {
		t.Fatalf("second page %v", rest)
	}

	// The other key sees only its own request; guests see nothing.
	theirs, err := approvalCall(t, tenant, other, "browseapprovals", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if items := wikiItems(theirs, "items"); len(items) != 1 || items[0]["id"] != elsewhere.ID {
		t.Fatalf("other sees %v", items)
	}
	if _, err := approvalCall(t, tenant, "", "browseapprovals", map[string]any{}); err == nil || !strings.HasPrefix(err.Error(), "auth-required:") {
		t.Fatalf("guest %v", err)
	}

	// One request with its history: the asked person and the asker may
	// read it, nobody else.
	detail, err := approvalCall(t, tenant, owner, "browseapproval", map[string]any{"id": decided.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := detail["item"].(map[string]any); got["state"] != "answered" || got["answer"].(map[string]any)["decision"] != "approved" {
		t.Fatalf("detail %v", detail)
	}
	if answers := detail["answers"].([]any); len(answers) != 2 || answers[0].(map[string]any)["id"] != approval.ID {
		t.Fatalf("answers %v", answers)
	}
	if _, err := approvalCall(t, tenant, asker, "browseapproval", map[string]any{"id": decided.ID}); err != nil {
		t.Fatalf("asker detail %v", err)
	}
	if _, err := approvalCall(t, tenant, other, "browseapproval", map[string]any{"id": decided.ID}); err == nil || !strings.HasPrefix(err.Error(), "restricted:") {
		t.Fatalf("stranger detail %v", err)
	}
	if _, err := approvalCall(t, tenant, owner, "browseapproval", map[string]any{"id": reply.ID}); err == nil || !strings.HasPrefix(err.Error(), "not found:") {
		t.Fatalf("reply as request %v", err)
	}
}

func TestApprovalRequestsWakeDevicesWithActionsAndCountOnBadge(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	asker := wikiKey(t, approvalAskerSecret)
	now := time.Now().Unix()

	request := event.Event{Kind: kindComment, PubKey: asker, CreatedAt: now, Tags: [][]string{{"request", "decide"}, {"p", owner}, {"subject", "Publish  release notes"}, {"expiration", strconv.FormatInt(now+3600, 10)}}, Content: "Long body"}
	notices := tenant.pushNotices(ctx, request)
	if len(notices) != 1 || notices[0].category != pushApprovals || notices[0].recipient != owner || notices[0].body != asker[:12]+" asks: Publish release notes" || !strings.HasSuffix(notices[0].url, "/approvals?id="+request.ID) {
		t.Fatalf("decision notices %+v", notices)
	}
	if actions := notices[0].actions; len(actions) != 3 || actions[0].Action != "approve" || actions[1].Action != "deny" || actions[2].Action != "reply" || actions[0].Title != "Approve" {
		t.Fatalf("decision actions %+v", notices[0].actions)
	}
	question := event.Event{Kind: kindChatMessage, PubKey: asker, CreatedAt: now, Tags: [][]string{{"request", "question"}, {"p", owner}, {"h", "build"}}, Content: "Mention the edge?"}
	notices = tenant.pushNotices(ctx, question)
	if len(notices) != 1 || notices[0].body != asker[:12]+" asks: Mention the edge?" || len(notices[0].actions) != 1 || notices[0].actions[0].Action != "reply" {
		t.Fatalf("question notices %+v", notices)
	}
	// A comment that asks nobody keeps its mention notice.
	plain := event.Event{Kind: kindComment, PubKey: asker, CreatedAt: now, Tags: [][]string{{"p", owner}}, Content: "Hello"}
	if notices := tenant.pushNotices(ctx, plain); len(notices) != 1 || notices[0].category != pushMentions {
		t.Fatalf("plain notices %+v", notices)
	}
	if !containsString(pushCategories, pushApprovals) {
		t.Fatal("approvals is not a device category")
	}

	// The actions travel through the payload to the device message.
	message := tenant.pushMessage(ctx, pushPayload{Recipient: owner, Kind: pushApprovals, Text: notices[0].body, URL: notices[0].url, Actions: notices[0].actions})
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"actions":[{"action":"reply","title":"Reply"}]`) || !strings.Contains(string(encoded), `"tag":"tiny-approvals"`) {
		t.Fatalf("message %s", encoded)
	}
	if encoded, _ := json.Marshal(tenant.pushMessage(ctx, pushPayload{Recipient: owner, Kind: pushMentions, Text: "hi"})); strings.Contains(string(encoded), "actions") {
		t.Fatalf("mention message carries actions: %s", encoded)
	}

	// The badge counts open requests once, whether they arrived before or
	// after the last inbox visit, and drops them once answered.
	if err := tenant.markInboxSeen(ctx, owner); err != nil {
		t.Fatal(err)
	}
	seen := tenant.inboxSeen(ctx, owner)
	older := wikiPublish(t, tenant, approvalAskerSecret, kindComment, seen-100, "Older request", []string{"request", "approve"}, []string{"p", owner})
	if got := tenant.inboxUnread(ctx, owner); got != 1 {
		t.Fatalf("unread with an older open request = %d", got)
	}
	newer := wikiPublish(t, tenant, approvalAskerSecret, kindComment, seen+100, "Newer request", []string{"request", "approve"}, []string{"p", owner})
	wikiPublish(t, tenant, approvalAskerSecret, kindChatMessage, seen+100, "Room request", []string{"request", "question"}, []string{"p", owner}, []string{"h", tenant.meta.Name})
	if got := tenant.inboxUnread(ctx, owner); got != 3 {
		t.Fatalf("unread with three open requests = %d", got)
	}
	wikiPublish(t, tenant, wikiOwnerSecret, kindReaction, seen+200, "+", []string{"e", older.ID}, []string{"p", asker})
	wikiPublish(t, tenant, wikiOwnerSecret, kindReaction, seen+200, "-", []string{"e", newer.ID}, []string{"p", asker})
	// The newer request still counts as a conversation event since the
	// visit; the older one is answered and gone from the badge.
	if got := tenant.inboxUnread(ctx, owner); got != 2 {
		t.Fatalf("unread after answering = %d", got)
	}
}

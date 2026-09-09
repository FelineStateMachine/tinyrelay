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

// jobTenant returns a tenant with a member who requests jobs and an agent
// granted to serve them.
func jobTenant(t *testing.T) (*App, *Tenant, string, string) {
	t.Helper()
	app, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	member, _ := event.PublicKey(testMemberSecret)
	agent, _ := event.PublicKey(testAgentSecret)
	if _, err := tenant.Execute(ctx, owner, "setmember", []json.RawMessage{json.RawMessage(strconv.Quote(member)), json.RawMessage(`{"role":"member"}`)}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if err := publishAs(t, tenant, agentGrantEvent(t, agent, now, now+3600, []string{"jobs", "serve"})); err != nil {
		t.Fatalf("grant rejected: %v", err)
	}
	return app, tenant, member, agent
}

func TestBrowseJobsListsRequestsWithStatusAndResults(t *testing.T) {
	_, tenant, member, agent := jobTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	now := time.Now().Unix()

	open := wikiPublish(t, tenant, testMemberSecret, 5001, now-400, "", []string{"i", "hello", "text"}, []string{"i", strings.Repeat("d", 64), "event", "wss://relay.example", "context"}, []string{"output", "text/plain"}, []string{"bid", "1000"}, []string{"param", "lang", "es"}, []string{"relays", "wss://a.example", "wss://b.example"}, []string{"p", agent})
	wikiPublish(t, tenant, testAgentSecret, 7000, now-350, "", []string{"e", open.ID}, []string{"p", member}, []string{"status", "payment-required"}, []string{"amount", "500", "lnbc1"})
	working := wikiPublish(t, tenant, testAgentSecret, 7000, now-300, "", []string{"e", open.ID}, []string{"p", member}, []string{"status", "processing", "halfway"})
	done := wikiPublish(t, tenant, testMemberSecret, 5002, now-390, "", []string{"i", "summarize", "text"})
	wikiPublish(t, tenant, testAgentSecret, 7000, now-380, "", []string{"e", done.ID}, []string{"p", member}, []string{"status", "processing"})
	result := wikiPublish(t, tenant, testAgentSecret, 6002, now-200, "the summary", []string{"e", done.ID}, []string{"p", member}, []string{"amount", "700"}, []string{"request", "{}"})
	failed := wikiPublish(t, tenant, testOwnerSecret, 5001, now-300, "", []string{"i", "broken", "text"})
	wikiPublish(t, tenant, testAgentSecret, 7000, now-250, "", []string{"e", failed.ID}, []string{"p", owner}, []string{"status", "error", "no such input"})
	// An expired request is saved directly, as it would have been before it
	// lapsed, and is no longer served.
	expired := event.Event{Kind: 5001, CreatedAt: now - 7200, Content: "", Tags: [][]string{{"expiration", strconv.FormatInt(now-3600, 10)}}}
	if err := event.Sign(&expired, testMemberSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.Save(ctx, expired, storageSave(tenant)); err != nil {
		t.Fatal(err)
	}
	note := wikiPublish(t, tenant, testMemberSecret, 1, now-100, "not a job")

	listed, err := approvalCall(t, tenant, member, "browsejobs", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	items := wikiItems(listed, "items")
	if len(items) != 3 || items[0]["id"] != failed.ID || items[1]["id"] != done.ID || items[2]["id"] != open.ID || listed["next_cursor"] != "" {
		t.Fatalf("listed %v", items)
	}
	item := approvalByID(items, open.ID)
	feedback, _ := item["feedback"].(map[string]any)
	if item["state"] != "open" || item["status"] != "processing" || item["kind"] != 5001.0 || item["requester"] != member || item["output"] != "text/plain" || item["bid"] != "1000" || item["result"] != nil || feedback == nil || feedback["id"] != working.ID || feedback["info"] != "halfway" || feedback["provider"] != agent {
		t.Fatalf("open item %v", item)
	}
	if inputs := item["inputs"].([]any); len(inputs) != 2 || inputs[0].(map[string]any)["type"] != "text" || inputs[1].(map[string]any)["relay"] != "wss://relay.example" || inputs[1].(map[string]any)["marker"] != "context" {
		t.Fatalf("inputs %v", item["inputs"])
	}
	if params := item["params"].([]any); len(params) != 1 || params[0].(map[string]any)["key"] != "lang" || params[0].(map[string]any)["value"] != "es" {
		t.Fatalf("params %v", item["params"])
	}
	if relays := item["relays"].([]any); len(relays) != 2 || relays[1] != "wss://b.example" {
		t.Fatalf("relays %v", item["relays"])
	}
	if providers := item["providers"].([]any); len(providers) != 1 || providers[0] != agent {
		t.Fatalf("providers %v", item["providers"])
	}
	item = approvalByID(items, done.ID)
	answer, _ := item["result"].(map[string]any)
	if item["state"] != "done" || item["status"] != "processing" || answer == nil || answer["id"] != result.ID || answer["kind"] != 6002.0 || answer["amount"] != "700" || answer["content"] != "the summary" {
		t.Fatalf("done item %v", item)
	}
	item = approvalByID(items, failed.ID)
	if item["state"] != "done" || item["status"] != "error" || item["result"] != nil {
		t.Fatalf("failed item %v", item)
	}

	for state, want := range map[string][]string{"open": {open.ID}, "done": {failed.ID, done.ID}} {
		filtered, err := approvalCall(t, tenant, member, "browsejobs", map[string]any{"state": state})
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, item := range wikiItems(filtered, "items") {
			got = append(got, item["id"].(string))
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("state %s lists %v", state, got)
		}
	}
	mine, err := approvalCall(t, tenant, member, "browsejobs", map[string]any{"mine": true})
	if err != nil {
		t.Fatal(err)
	}
	if items := wikiItems(mine, "items"); len(items) != 2 || approvalByID(items, failed.ID) != nil {
		t.Fatalf("mine %v", items)
	}
	if _, err := approvalCall(t, tenant, member, "browsejobs", map[string]any{"state": "bogus"}); err == nil || !strings.HasPrefix(err.Error(), "invalid:") {
		t.Fatalf("bogus state %v", err)
	}
	if _, err := approvalCall(t, tenant, "", "browsejobs", map[string]any{"mine": true}); err == nil || !strings.HasPrefix(err.Error(), "auth-required:") {
		t.Fatalf("guest mine %v", err)
	}
	// Guests see the requests on an open relay; pages follow the cursor.
	paged, err := approvalCall(t, tenant, "", "browsejobs", map[string]any{"limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	if items := wikiItems(paged, "items"); len(items) != 2 || paged["next_cursor"] == "" {
		t.Fatalf("paged %v", paged)
	}
	rest, err := approvalCall(t, tenant, "", "browsejobs", map[string]any{"limit": 2, "cursor": paged["next_cursor"]})
	if err != nil {
		t.Fatal(err)
	}
	if items := wikiItems(rest, "items"); len(items) != 1 || items[0]["id"] != open.ID || rest["next_cursor"] != "" {
		t.Fatalf("second page %v", rest)
	}

	detail, err := approvalCall(t, tenant, member, "browsejob", map[string]any{"id": open.ID})
	if err != nil {
		t.Fatal(err)
	}
	timeline := wikiItems(detail, "feedback")
	if item := detail["item"].(map[string]any); item["state"] != "open" || len(timeline) != 2 || timeline[0]["status"] != "payment-required" || timeline[0]["invoice"] != "lnbc1" || timeline[1]["id"] != working.ID || len(detail["results"].([]any)) != 0 {
		t.Fatalf("open detail %v", detail)
	}
	detail, err = approvalCall(t, tenant, member, "browsejob", map[string]any{"id": done.ID})
	if err != nil {
		t.Fatal(err)
	}
	if results := wikiItems(detail, "results"); len(results) != 1 || results[0]["id"] != result.ID || len(wikiItems(detail, "feedback")) != 1 {
		t.Fatalf("done detail %v", detail)
	}
	for _, id := range []string{note.ID, expired.ID, strings.Repeat("f", 64)} {
		if _, err := approvalCall(t, tenant, member, "browsejob", map[string]any{"id": id}); err == nil || !strings.HasPrefix(err.Error(), "not found:") {
			t.Fatalf("browsejob %s: %v", id, err)
		}
	}
	// Switching long tasks off closes the browse surface too.
	policy := tenant.Policy()
	policy.Features.Jobs = false
	if err := tenant.applyPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	if _, err := approvalCall(t, tenant, member, "browsejobs", map[string]any{}); err == nil || !strings.Contains(err.Error(), "switched off") {
		t.Fatalf("browse with jobs off %v", err)
	}
}

package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func approvalGrantRequest(t *testing.T, grant event.Event, createdAt int64) event.Event {
	t.Helper()
	raw, err := json.Marshal(community.AgentGrantRequest{Base: grant.ID, Changes: community.AgentGrantChanges{Kinds: []int{30617, 30618}, Repos: []community.AgentRepo{{Owner: event.Tag(grant, "d"), Identifier: "seedmark", Level: "maintain"}}}})
	if err != nil {
		t.Fatal(err)
	}
	return signedEvent(t, testAgentSecret, 1111, createdAt, [][]string{{"request", "grant"}, {"grant", string(raw)}, {"E", grant.ID, "", grant.PubKey}, {"K", "30392"}, {"P", grant.PubKey}, {"e", grant.ID, "", grant.PubKey}, {"k", "30392"}, {"p", grant.PubKey}}, "Publish the Seedmark repository and its hosting manifest.")
}

func TestGrantApprovalAppliesAndRetainsDecision(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	now := time.Now().Unix()
	agent, _ := event.PublicKey(testAgentSecret)
	grant := agentGrantEvent(t, agent, now-30, now+3600, []string{"k", "9"}, []string{"room", tenant.meta.Name}, []string{"sites", "*"})
	if err := publishAs(t, tenant, grant); err != nil {
		t.Fatal(err)
	}
	request := approvalGrantRequest(t, grant, now-20)
	if err := publishAs(t, tenant, request); err != nil {
		t.Fatalf("request with no comment scope: %v", err)
	}
	detail, err := approvalCall(t, tenant, grant.PubKey, "browseapproval", map[string]any{"id": request.ID})
	if err != nil {
		t.Fatal(err)
	}
	item := detail["item"].(map[string]any)
	if item["type"] != "grant" || item["state"] != "open" || item["grant"] == nil {
		t.Fatalf("missing actionable review: %v", item)
	}
	for _, actor := range []string{agent, grant.PubKey} {
		if _, err := approvalCall(t, tenant, actor, "browseapproval", map[string]any{"id": request.ID}); err != nil {
			t.Fatalf("participant cannot read review: %v", err)
		}
	}
	if _, err := approvalCall(t, tenant, wikiKey(t, approvalOtherSecret), "browseapproval", map[string]any{"id": request.ID}); err == nil {
		t.Fatal("stranger read grant request")
	}
	// A reaction alone must never make the UI claim access was granted.
	positive := signedEvent(t, testOwnerSecret, 7, now-10, [][]string{{"e", request.ID}, {"p", agent}}, "+")
	if err := publishAs(t, tenant, positive); err != nil {
		t.Fatal(err)
	}
	detail, err = approvalCall(t, tenant, grant.PubKey, "browseapproval", map[string]any{"id": request.ID})
	if err != nil || detail["item"].(map[string]any)["state"] != "open" {
		t.Fatalf("reaction settled an unapplied grant: %v %v", detail, err)
	}
	review, err := tenant.community.ReviewAgentGrantRequest(ctx, request, now)
	if err != nil {
		t.Fatal(err)
	}
	replacement := review.Unsigned
	if err := event.Sign(&replacement, testOwnerSecret); err != nil {
		t.Fatal(err)
	}
	if err := publishAs(t, tenant, replacement); err != nil {
		t.Fatalf("reviewed replacement rejected: %v", err)
	}
	current, found, err := tenant.community.AgentGrant(ctx, agent)
	if err != nil || !found || !containsInt(current.Scope.Kinds, 30617) || len(current.Scope.Sites) != 1 {
		t.Fatalf("grant not applied or existing site lost: %+v %v", current, err)
	}
	// A later ordinary edit must not erase the approval's receipt.
	manual := agentGrantEvent(t, agent, now+1, now+3600, []string{"k", "9"})
	if err := publishAs(t, tenant, manual); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"browseapproval", "browseapprovals"} {
		result, err := approvalCall(t, tenant, grant.PubKey, method, map[string]any{"id": request.ID})
		if err != nil {
			t.Fatal(err)
		}
		got := result["item"]
		if method == "browseapprovals" {
			got = approvalByID(wikiItems(result, "items"), request.ID)
		}
		item := got.(map[string]any)
		answer := item["answer"].(map[string]any)
		if item["state"] != "answered" || answer["decision"] != "approved" || answer["id"] != replacement.ID {
			t.Fatalf("lost grant approval history: %v", item)
		}
	}
}

func TestGrantApprovalDenialAndStaleReview(t *testing.T) {
	for _, scenario := range []string{"deny", "supersede"} {
		t.Run(scenario, func(t *testing.T) {
			_, tenant := testTenant(t)
			now := time.Now().Unix()
			agent, _ := event.PublicKey(testAgentSecret)
			grant := agentGrantEvent(t, agent, now-30, now+3600, []string{"k", "9"})
			if err := publishAs(t, tenant, grant); err != nil {
				t.Fatal(err)
			}
			request := approvalGrantRequest(t, grant, now-20)
			if err := publishAs(t, tenant, request); err != nil {
				t.Fatal(err)
			}
			review, err := tenant.community.ReviewAgentGrantRequest(context.Background(), request, now)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "deny" {
				if err := publishAs(t, tenant, signedEvent(t, testOwnerSecret, 7, now-10, [][]string{{"e", request.ID}, {"p", agent}}, "-")); err != nil {
					t.Fatal(err)
				}
			} else if err := publishAs(t, tenant, agentGrantEvent(t, agent, now-5, now+3600, []string{"k", "1"})); err != nil {
				t.Fatal(err)
			}
			replacement := review.Unsigned
			if err := event.Sign(&replacement, testOwnerSecret); err != nil {
				t.Fatal(err)
			}
			if err := publishAs(t, tenant, replacement); err == nil {
				t.Fatal("obsolete request changed the grant")
			}
			detail, err := approvalCall(t, tenant, grant.PubKey, "browseapproval", map[string]any{"id": request.ID})
			if err != nil {
				t.Fatal(err)
			}
			item := detail["item"].(map[string]any)
			if item["grant"] != nil {
				t.Fatalf("inactive request still actionable: %v", item)
			}
			if scenario == "deny" && (item["state"] != "answered" || item["answer"].(map[string]any)["decision"] != "denied") {
				t.Fatalf("denial not shown: %v", item)
			}
			if scenario == "supersede" && (item["state"] != "expired" || !strings.HasPrefix(item["grant_error"].(string), "conflict:")) {
				t.Fatalf("stale review not marked: %v", item)
			}
		})
	}
}

func TestGrantRequestNotificationRequiresReview(t *testing.T) {
	_, tenant := testTenant(t)
	now := time.Now().Unix()
	agent, _ := event.PublicKey(testAgentSecret)
	request := approvalGrantRequest(t, agentGrantEvent(t, agent, now-30, now+3600, []string{"k", "9"}), now-20)
	notices := tenant.pushNotices(context.Background(), request)
	if len(notices) != 1 || notices[0].recipient != tenant.Policy().Owner || notices[0].category != pushApprovals || len(notices[0].actions) != 1 || notices[0].actions[0].Action != "review" || !strings.Contains(notices[0].url, "/approvals?id="+request.ID) {
		t.Fatalf("grant request notification: %+v", notices)
	}
}

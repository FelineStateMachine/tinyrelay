package webui

import (
	"strings"
	"testing"
)

func TestChatActivityViewScopesDecisionsAndPreservesJobSummary(t *testing.T) {
	actor := strings.Repeat("a", 64)
	other := strings.Repeat("b", 64)
	jobs := map[string]any{"items": []any{map[string]any{"id": strings.Repeat("1", 64), "requester": other, "status": "working", "content": "Indexing files"}}}
	approvals := map[string]any{"items": []any{
		map[string]any{"id": strings.Repeat("2", 64), "asker": other, "asked": []any{actor}, "subject": "Publish result", "state": "open", "kind": "4550"},
		map[string]any{"id": strings.Repeat("3", 64), "asker": other, "asked": []any{other}, "subject": "Private request", "state": "open", "kind": "4550"},
	}}
	items := chatActivityView(jobs, approvals, actor, "/chat/activity", "general")
	if len(items) != 2 {
		t.Fatalf("got %d activity items, want job and addressed approval", len(items))
	}
	if items[0].Type != "job" || items[0].Status != "working" || items[0].Title != "Indexing files" {
		t.Fatalf("unexpected job card: %#v", items[0])
	}
	if items[1].Type != "approval" || !items[1].CanDecide || items[1].ID != strings.Repeat("2", 64) {
		t.Fatalf("unexpected approval card: %#v", items[1])
	}
}

func TestChatActivityViewDisablesAnsweredApproval(t *testing.T) {
	actor := strings.Repeat("a", 64)
	approvals := map[string]any{"items": []any{map[string]any{"id": strings.Repeat("2", 64), "asker": actor, "asked": []any{actor}, "content": "Done", "state": "answered"}}}
	items := chatActivityView(nil, approvals, actor, "/chat/activity", "general")
	if len(items) != 1 || items[0].CanDecide {
		t.Fatalf("answered approval remained actionable: %#v", items)
	}
}

func TestChatActivityAskerCannotApproveOwnRequest(t *testing.T) {
	actor := strings.Repeat("a", 64)
	approvals := map[string]any{"items": []any{map[string]any{"id": strings.Repeat("2", 64), "asker": actor, "asked": []string{actor, strings.Repeat("b", 64)}, "state": "open"}}}
	items := chatActivityView(nil, approvals, actor, "/chat/activity", "main")
	if len(items) != 1 || items[0].CanDecide {
		t.Fatalf("asker allowed to decide: %#v", items)
	}
}

func TestChatActivityPreservesAnsweredQuestion(t *testing.T) {
	actor := strings.Repeat("a", 64)
	other := strings.Repeat("b", 64)
	approvals := map[string]any{"items": []any{map[string]any{
		"id": strings.Repeat("4", 64), "asker": other, "asked": []string{actor},
		"type": "question", "subject": "Which preview?", "state": "answered",
		"answer": map[string]any{"decision": "replied", "content": "Use the dark preview."},
	}}}
	items := chatActivityView(nil, approvals, actor, "/chat/activity", "general")
	if len(items) != 1 || items[0].Answer != "Use the dark preview." || items[0].CanDecide {
		t.Fatalf("answered question = %#v", items)
	}
}

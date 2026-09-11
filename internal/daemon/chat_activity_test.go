package daemon

import (
	"encoding/json"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"testing"
)

func TestChatActivitySharesApprovalStateAndScopesRooms(t *testing.T) {
	h := newRoomHarness(t)
	request := h.must("alice", 5000, [][]string{{"h", "main"}, {"p", h.keys["bob"]}, {"subject", "Build preview"}}, "Build preview")
	h.must("bob", 7000, [][]string{{"h", "main"}, {"e", request.ID}, {"p", h.keys["alice"]}, {"status", "processing", "Running checks"}}, "2 of 3 checks complete")
	decision := h.must("alice", 9, [][]string{{"h", "main"}, {"request", "approve"}, {"p", h.keys["bob"]}, {"subject", "Publish preview"}}, "Publish it?")
	read := func(who, room string) map[string]any {
		t.Helper()
		raw, err := h.tenant.Execute(h.ctx, h.keys[who], "browsechatactivity", []json.RawMessage{rawJSON(map[string]string{"room": room})})
		if err != nil {
			t.Fatal(err)
		}
		return raw.(map[string]any)
	}
	data := read("bob", "main")
	jobs := data["jobs"].([]jobItem)
	if len(jobs) != 1 || jobs[0].Status != "processing" {
		t.Fatalf("jobs=%v", jobs)
	}
	approvals := data["approvals"].([]approvalItem)
	if len(approvals) != 1 || approvals[0].State != "open" {
		t.Fatalf("approvals=%v", approvals)
	}
	h.must("bob", 7, [][]string{{"h", "main"}, {"e", decision.ID}, {"p", h.keys["alice"]}}, "+")
	data = read("bob", "main")
	if data["approvals"].([]approvalItem)[0].State != "answered" {
		t.Fatal("inline approval not settled")
	}
	detail, err := h.tenant.Execute(h.ctx, h.keys["bob"], "browseapproval", []json.RawMessage{rawJSON(map[string]string{"id": decision.ID})})
	if err != nil {
		t.Fatal(err)
	}
	if detail.(map[string]any)["item"].(approvalItem).State != "answered" {
		t.Fatal("Approvals disagrees")
	}
	h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "private-chat"}, {"name", "Private"}, {"visibility", "members"}}, "")
	private := h.must("alice", 5000, [][]string{{"h", "private-chat"}, {"p", h.keys["alice"]}}, "Private work")
	if _, err := h.tenant.Execute(h.ctx, h.keys["bob"], "browsechatactivity", []json.RawMessage{rawJSON(map[string]string{"room": "private-chat"})}); err == nil {
		t.Fatal("outsider read private activity")
	}
	for _, job := range read("bob", "main")["jobs"].([]jobItem) {
		if job.ID == private.ID {
			t.Fatal("private job leaked")
		}
	}
	if _, err := h.publish("alice", 7000, [][]string{{"e", private.ID}, {"p", h.keys["alice"]}, {"status", "processing"}}, "private progress"); err == nil {
		t.Fatal("unscoped progress accepted for a private room task")
	}
}
func TestChatPresenceIsEphemeralAndRoomAuthorized(t *testing.T) {
	h := newRoomHarness(t)
	h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "private-chat"}, {"visibility", "members"}}, "")
	for _, kind := range []int{20001, 20002} {
		e := h.must("alice", kind, [][]string{{"h", "private-chat"}}, "typing")
		if rows := h.query("alice", event.Filter{IDs: []string{e.ID}}); len(rows) != 0 {
			t.Fatal("presence persisted")
		}
		if _, err := h.publish("bob", kind, [][]string{{"h", "private-chat"}}, "typing"); err == nil {
			t.Fatal("outsider presence accepted")
		}
	}
}

func TestChatActivityIncludesParentOnlyRequestsInCanonicalThread(t *testing.T) {
	h := newRoomHarness(t)
	root := h.must("alice", 9, [][]string{{"h", "main"}}, "Root")
	parent := h.must("bob", 9, [][]string{{"h", "main"}, {"e", root.ID, "", "reply"}}, "Parent")
	approval := h.must("alice", 9, [][]string{{"h", "main"}, {"e", parent.ID, "", "reply"}, {"request", "approve"}, {"p", h.keys["bob"]}}, "Approve the nested action?")
	job := h.must("alice", 5000, [][]string{{"h", "main"}, {"e", parent.ID}, {"p", h.keys["bob"]}}, "Nested job")
	other := h.must("alice", 9, [][]string{{"h", "main"}}, "Another root")
	h.must("alice", 9, [][]string{{"h", "main"}, {"e", other.ID, "", "root"}, {"request", "approve"}, {"p", h.keys["bob"]}}, "Unrelated action")
	raw, err := h.tenant.Execute(h.ctx, h.keys["bob"], "browsechatactivity", []json.RawMessage{rawJSON(map[string]string{"room": "main", "root": root.ID})})
	if err != nil {
		t.Fatal(err)
	}
	data := raw.(map[string]any)
	approvals, jobs := data["approvals"].([]approvalItem), data["jobs"].([]jobItem)
	if len(approvals) != 1 || approvals[0].ID != approval.ID || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("activity omitted nested request or included another thread: %v", data)
	}
}

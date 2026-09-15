package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/mcp"
)

// jobTestRoom is the test tenant's own room, open to every member.
const jobTestRoom = "main"

// jobTenant returns a tenant with a member who requests jobs and an agent
// granted to serve them in the tenant's room.
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
	if err := publishAs(t, tenant, agentGrantEvent(t, agent, now, now+3600, []string{"room", jobTestRoom}, []string{"jobs", "serve"})); err != nil {
		t.Fatalf("grant rejected: %v", err)
	}
	return app, tenant, member, agent
}

// jobRequestTags builds the tags of a request in the test room asking the
// given keys.
func jobRequestTags(asked []string, extra ...[]string) [][]string {
	tags := [][]string{{"h", jobTestRoom}}
	for _, key := range asked {
		tags = append(tags, []string{"p", key})
	}
	return append(tags, extra...)
}

// jobAnswerTags builds the tags of an answer to a request in the test room.
func jobAnswerTags(request event.Event, extra ...[]string) [][]string {
	return append([][]string{{"h", jobTestRoom}, {"e", request.ID}, {"p", request.PubKey}}, extra...)
}

func TestMCPJobToolsBuildValidateAndPublish(t *testing.T) {
	app, _, member, agent := jobTenant(t)
	now := time.Now().Unix()
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
	text := func(result map[string]any) string {
		content, _ := result["content"].([]any)
		if len(content) == 0 {
			return ""
		}
		return content[0].(map[string]any)["text"].(string)
	}
	tagsOf := func(result map[string]any) (map[string]any, string) {
		unsigned, _ := structured(result)["unsigned"].(map[string]any)
		tags, _ := json.Marshal(unsigned["tags"])
		return unsigned, string(tags)
	}
	_, response := mcpCall{method: "tools/list", sign: true}.do(t, app, "/mcp")
	names := map[string]bool{}
	for _, tool := range response.Result.(map[string]any)["tools"].([]any) {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"list_jobs", "read_job", "request_job", "accept_job", "job_progress", "job_result", "job_error", "cancel_job"} {
		if !names[want] {
			t.Fatalf("missing tool %s", want)
		}
	}

	// request_job: the template carries the room, the asked keys, the
	// subject, the thread root and the expiration; it refuses a request
	// without a room or an asked key, and the signed request publishes.
	root := strings.Repeat("d", 64)
	expires := strconv.FormatInt(now+3600, 10)
	result, isError := call("request_job", map[string]any{"room": jobTestRoom, "assignees": []any{agent}, "subject": "Summarize the notes", "content": "Summarize this week's notes.", "root": root, "expiration": now + 3600}, testMemberSecret)
	unsigned, tags := tagsOf(result)
	if isError || unsigned["kind"] != float64(event.KIND_JOB_REQUEST) || unsigned["content"] != "Summarize this week's notes." || tags != `[["h","main"],["p","`+agent+`"],["subject","Summarize the notes"],["e","`+root+`","","root"],["expiration","`+expires+`"]]` {
		t.Fatalf("request_job template: %v %s %s", isError, text(result), tags)
	}
	if result, isError = call("request_job", map[string]any{"assignees": []any{agent}, "content": "x"}, testMemberSecret); !isError || !strings.Contains(text(result), "room is required") {
		t.Fatalf("request_job without a room: %s", text(result))
	}
	if result, isError = call("request_job", map[string]any{"room": jobTestRoom, "content": "x"}, testMemberSecret); !isError || !strings.Contains(text(result), "at least one key") {
		t.Fatalf("request_job without assignees: %s", text(result))
	}
	if result, isError = call("request_job", map[string]any{"room": jobTestRoom, "assignees": []any{agent}}, testMemberSecret); !isError || !strings.Contains(text(result), "content or subject") {
		t.Fatalf("request_job without text: %s", text(result))
	}
	if result, isError = call("request_job", map[string]any{"event": mcpSigned(t, testMemberSecret, event.KIND_JOB_REQUEST, [][]string{{"h", jobTestRoom}}, "hello")}, testMemberSecret); !isError || !strings.Contains(text(result), "at least one asked key") || !strings.Contains(text(result), "Expected a signed kind 43001 event") {
		t.Fatalf("request_job malformed signed: %s", text(result))
	}
	request := mcpSigned(t, testMemberSecret, event.KIND_JOB_REQUEST, jobRequestTags([]string{agent}, []string{"subject", "Summarize the notes"}), "Summarize this week's notes.")
	result, isError = call("request_job", map[string]any{"event": request}, testMemberSecret)
	requestID, _ := structured(result)["event_id"].(string)
	if isError || structured(result)["accepted"] != true || requestID == "" {
		t.Fatalf("request_job publish: %s", text(result))
	}

	// accept_job and job_progress: the template takes the request event or
	// its fields, and the signed answer publishes only from an asked key.
	result, isError = call("accept_job", map[string]any{"request": request}, testAgentSecret)
	unsigned, tags = tagsOf(result)
	if isError || unsigned["kind"] != float64(event.KIND_JOB_ACCEPTED) || unsigned["content"] != "" || tags != `[["h","main"],["e","`+requestID+`"],["p","`+member+`"]]` {
		t.Fatalf("accept_job template: %v %s %s", isError, text(result), tags)
	}
	if result, isError = call("accept_job", map[string]any{"request": request, "room": "other"}, testAgentSecret); !isError || !strings.Contains(text(result), "must match the request") {
		t.Fatalf("accept_job mismatched room: %s", text(result))
	}
	if result, isError = call("job_progress", map[string]any{"e": requestID, "p": member, "room": jobTestRoom}, testAgentSecret); !isError || !strings.Contains(text(result), "content is required") {
		t.Fatalf("job_progress without content: %s", text(result))
	}
	result, isError = call("job_progress", map[string]any{"e": requestID, "p": member, "room": jobTestRoom, "content": "halfway"}, testAgentSecret)
	unsigned, tags = tagsOf(result)
	if isError || unsigned["kind"] != float64(event.KIND_JOB_PROGRESS) || unsigned["content"] != "halfway" || tags != `[["h","main"],["e","`+requestID+`"],["p","`+member+`"]]` {
		t.Fatalf("job_progress template: %v %s %s", isError, text(result), tags)
	}
	if result, isError = call("job_progress", map[string]any{"event": mcpSigned(t, testAgentSecret, event.KIND_JOB_PROGRESS, [][]string{{"e", requestID}, {"p", member}}, "halfway")}, testAgentSecret); !isError || !strings.Contains(text(result), "room in an h tag") {
		t.Fatalf("job_progress malformed signed: %s", text(result))
	}
	requestEvent := event.Event{ID: requestID, PubKey: member}
	result, isError = call("job_progress", map[string]any{"event": mcpSigned(t, testAgentSecret, event.KIND_JOB_PROGRESS, jobAnswerTags(requestEvent), "halfway")}, testAgentSecret)
	if isError || structured(result)["accepted"] != true {
		t.Fatalf("job_progress publish: %s", text(result))
	}
	if result, isError = call("job_progress", map[string]any{"event": mcpSigned(t, testOwnerSecret, event.KIND_JOB_PROGRESS, jobAnswerTags(requestEvent), "not asked")}, testOwnerSecret); !isError || !strings.Contains(text(result), "key the request asked") {
		t.Fatalf("job_progress from a key not asked: %s", text(result))
	}

	// job_result: the template carries the artifacts and refuses bad ones;
	// the relay checks the answer against the request it holds.
	artifacts := []any{map[string]any{"type": "e", "value": root}, map[string]any{"type": "a", "value": "30818:" + member + ":notes"}, map[string]any{"type": "r", "value": "https://example.com/report"}}
	result, isError = call("job_result", map[string]any{"request": request, "content": "the summary", "artifacts": artifacts}, testAgentSecret)
	unsigned, tags = tagsOf(result)
	if isError || unsigned["kind"] != float64(event.KIND_JOB_RESULT) || unsigned["content"] != "the summary" || tags != `[["h","main"],["e","`+requestID+`"],["p","`+member+`"],["e","`+root+`"],["a","30818:`+member+`:notes"],["r","https://example.com/report"]]` {
		t.Fatalf("job_result template: %v %s %s", isError, text(result), tags)
	}
	if result, isError = call("job_result", map[string]any{"request": request, "content": "x", "artifacts": []any{map[string]any{"type": "r", "value": "ftp://example.com"}}}, testAgentSecret); !isError || !strings.Contains(text(result), "each artifact") {
		t.Fatalf("job_result bad artifact: %s", text(result))
	}
	if result, isError = call("job_result", map[string]any{"e": requestID, "p": member, "room": jobTestRoom}, testAgentSecret); !isError || !strings.Contains(text(result), "content is required") {
		t.Fatalf("job_result without content: %s", text(result))
	}
	if result, isError = call("job_result", map[string]any{"event": mcpSigned(t, testAgentSecret, event.KIND_JOB_RESULT, [][]string{{"h", jobTestRoom}, {"e", strings.Repeat("e", 64)}, {"p", member}}, "x")}, testAgentSecret); !isError || !strings.Contains(text(result), "request this relay holds") {
		t.Fatalf("job_result for an unknown request: %s", text(result))
	}
	result, isError = call("job_result", map[string]any{"event": mcpSigned(t, testAgentSecret, event.KIND_JOB_RESULT, jobAnswerTags(requestEvent, []string{"r", "https://example.com/report"}), "the summary")}, testAgentSecret)
	resultID, _ := structured(result)["event_id"].(string)
	if isError || structured(result)["accepted"] != true || resultID == "" {
		t.Fatalf("job_result publish: %s", text(result))
	}

	// job_error and cancel_job build their events; only the requester
	// cancels.
	result, isError = call("job_error", map[string]any{"request": request, "content": "no such input"}, testAgentSecret)
	unsigned, tags = tagsOf(result)
	if isError || unsigned["kind"] != float64(event.KIND_JOB_ERROR) || unsigned["content"] != "no such input" || tags != `[["h","main"],["e","`+requestID+`"],["p","`+member+`"]]` {
		t.Fatalf("job_error template: %v %s %s", isError, text(result), tags)
	}
	result, isError = call("cancel_job", map[string]any{"request": request, "p": agent, "content": "never mind"}, testMemberSecret)
	unsigned, tags = tagsOf(result)
	if isError || unsigned["kind"] != float64(event.KIND_JOB_CANCEL) || unsigned["content"] != "never mind" || tags != `[["h","main"],["e","`+requestID+`"],["p","`+agent+`"]]` {
		t.Fatalf("cancel_job template: %v %s %s", isError, text(result), tags)
	}
	if result, isError = call("cancel_job", map[string]any{"event": mcpSigned(t, testOwnerSecret, event.KIND_JOB_CANCEL, [][]string{{"h", jobTestRoom}, {"e", requestID}}, "")}, testOwnerSecret); !isError || !strings.Contains(text(result), "only the requester") {
		t.Fatalf("cancel_job from another key: %s", text(result))
	}

	// The reads show the settled request.
	result, isError = call("list_jobs", map[string]any{"state": "done", "mine": true}, testMemberSecret)
	items, _ := structured(result)["items"].([]any)
	if isError || len(items) != 1 {
		t.Fatalf("list_jobs: %s", text(result))
	}
	item := items[0].(map[string]any)
	answer, _ := item["result"].(map[string]any)
	if item["id"] != requestID || item["status"] != "done" || item["subject"] != "Summarize the notes" || answer["id"] != resultID || answer["artifacts"].([]any)[0].(map[string]any)["value"] != "https://example.com/report" {
		t.Fatalf("list_jobs item: %v", item)
	}
	result, isError = call("read_job", map[string]any{"id": requestID}, testMemberSecret)
	if isError || len(structured(result)["answers"].([]any)) != 2 {
		t.Fatalf("read_job: %s", text(result))
	}
	r := httptest.NewRequest(http.MethodGet, "http://relay.test/llms.txt", nil)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "request_job, accept_job, job_progress, job_result, job_error and cancel_job") || !strings.Contains(w.Body.String(), "list_jobs and read_job") || strings.Contains(w.Body.String(), "NIP-90") {
		t.Fatalf("llms.txt: %s", w.Body.String())
	}
}

func TestJobAnswersWakeTheRequester(t *testing.T) {
	_, tenant, member, agent := jobTenant(t)
	ctx := context.Background()
	now := time.Now().Unix()
	request := wikiPublish(t, tenant, testMemberSecret, event.KIND_JOB_REQUEST, now-100, "Summarize this week's notes.", jobRequestTags([]string{agent}, []string{"subject", "Summarize the notes"})...)
	untitled := wikiPublish(t, tenant, testMemberSecret, event.KIND_JOB_REQUEST, now-90, "Build the preview.", jobRequestTags([]string{agent})...)
	result := event.Event{Kind: event.KIND_JOB_RESULT, PubKey: agent, CreatedAt: now, Tags: jobAnswerTags(request), Content: "the summary"}
	notices := tenant.pushNotices(ctx, result)
	if len(notices) != 1 || notices[0].category != pushMentions || notices[0].recipient != member || notices[0].body != "task Summarize the notes done" || !strings.HasSuffix(notices[0].url, "/e/"+result.ID) || len(notices[0].actions) != 0 {
		t.Fatalf("result notices %+v", notices)
	}
	failed := event.Event{Kind: event.KIND_JOB_ERROR, PubKey: agent, CreatedAt: now, Tags: jobAnswerTags(untitled), Content: "no such input"}
	if notices := tenant.pushNotices(ctx, failed); len(notices) != 1 || notices[0].category != pushMentions || notices[0].body != "task "+untitled.ID[:8]+" failed" {
		t.Fatalf("error notices %+v", notices)
	}
	for _, kind := range []int{event.KIND_JOB_ACCEPTED, event.KIND_JOB_PROGRESS} {
		quiet := event.Event{Kind: kind, PubKey: agent, CreatedAt: now, Tags: jobAnswerTags(request), Content: "halfway"}
		if notices := tenant.pushNotices(ctx, quiet); len(notices) != 0 {
			t.Fatalf("kind %d notices %+v", kind, notices)
		}
	}
	// The requester's own cancel wakes nobody, and neither does a result
	// the requester publishes.
	cancel := event.Event{Kind: event.KIND_JOB_CANCEL, PubKey: member, CreatedAt: now, Tags: [][]string{{"h", jobTestRoom}, {"e", request.ID}, {"p", agent}}}
	if notices := tenant.pushNotices(ctx, cancel); len(notices) != 0 {
		t.Fatalf("cancel notices %+v", notices)
	}
	own := event.Event{Kind: event.KIND_JOB_RESULT, PubKey: member, CreatedAt: now, Tags: jobAnswerTags(request), Content: "x"}
	if notices := tenant.pushNotices(ctx, own); len(notices) != 0 {
		t.Fatalf("own notices %+v", notices)
	}
}

func TestBrowseJobsListsRequestsWithStateAndAnswers(t *testing.T) {
	_, tenant, member, agent := jobTenant(t)
	ctx := context.Background()
	now := time.Now().Unix()
	root := wikiPublish(t, tenant, testMemberSecret, event.KIND_CHAT, now-500, "Could you summarize the notes?", []string{"h", jobTestRoom})

	open := wikiPublish(t, tenant, testMemberSecret, event.KIND_JOB_REQUEST, now-400, "Summarize this week's notes.", jobRequestTags([]string{agent}, []string{"subject", "Summarize the notes"}, []string{"e", root.ID, "", "root"})...)
	wikiPublish(t, tenant, testAgentSecret, event.KIND_JOB_ACCEPTED, now-350, "", jobAnswerTags(open)...)
	working := wikiPublish(t, tenant, testAgentSecret, event.KIND_JOB_PROGRESS, now-300, "halfway", jobAnswerTags(open)...)
	done := wikiPublish(t, tenant, testMemberSecret, event.KIND_JOB_REQUEST, now-390, "Build the preview.", jobRequestTags([]string{agent})...)
	wikiPublish(t, tenant, testAgentSecret, event.KIND_JOB_PROGRESS, now-380, "building", jobAnswerTags(done)...)
	result := wikiPublish(t, tenant, testAgentSecret, event.KIND_JOB_RESULT, now-200, "the preview", jobAnswerTags(done, []string{"e", root.ID}, []string{"a", "30818:" + member + ":notes"}, []string{"r", "https://example.com/preview"})...)
	failed := wikiPublish(t, tenant, testOwnerSecret, event.KIND_JOB_REQUEST, now-300, "Transcribe the call.", jobRequestTags([]string{agent})...)
	wikiPublish(t, tenant, testAgentSecret, event.KIND_JOB_ERROR, now-250, "no such input", jobAnswerTags(failed)...)
	cancelled := wikiPublish(t, tenant, testMemberSecret, event.KIND_JOB_REQUEST, now-280, "Never mind this one.", jobRequestTags([]string{agent})...)
	wikiPublish(t, tenant, testAgentSecret, event.KIND_JOB_ACCEPTED, now-270, "", jobAnswerTags(cancelled)...)
	wikiPublish(t, tenant, testMemberSecret, event.KIND_JOB_CANCEL, now-260, "", []string{"h", jobTestRoom}, []string{"e", cancelled.ID}, []string{"p", agent})
	// A late progress line does not reopen a cancelled task.
	wikiPublish(t, tenant, testAgentSecret, event.KIND_JOB_PROGRESS, now-255, "still going", jobAnswerTags(cancelled)...)
	// An expired request is saved directly, as it would have been before it
	// lapsed, and is no longer served.
	expired := event.Event{Kind: event.KIND_JOB_REQUEST, CreatedAt: now - 7200, Content: "old", Tags: jobRequestTags([]string{agent}, []string{"expiration", strconv.FormatInt(now-3600, 10)})}
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
	if len(items) != 4 || items[0]["id"] != cancelled.ID || items[1]["id"] != failed.ID || items[2]["id"] != done.ID || items[3]["id"] != open.ID || listed["next_cursor"] != "" {
		t.Fatalf("listed %v", items)
	}
	item := approvalByID(items, open.ID)
	progress, _ := item["progress"].(map[string]any)
	if item["state"] != "open" || item["status"] != "running" || item["room"] != jobTestRoom || item["requester"] != member || item["subject"] != "Summarize the notes" || item["content"] != "Summarize this week's notes." || item["root"] != root.ID || item["result"] != nil || item["error"] != nil || progress == nil || progress["id"] != working.ID || progress["content"] != "halfway" || progress["provider"] != agent {
		t.Fatalf("open item %v", item)
	}
	if assignees := item["assignees"].([]any); len(assignees) != 1 || assignees[0] != agent {
		t.Fatalf("assignees %v", item["assignees"])
	}
	item = approvalByID(items, done.ID)
	answer, _ := item["result"].(map[string]any)
	artifacts, _ := answer["artifacts"].([]any)
	if item["state"] != "done" || item["status"] != "done" || item["subject"] != nil || answer["id"] != result.ID || answer["kind"] != float64(event.KIND_JOB_RESULT) || answer["content"] != "the preview" || len(artifacts) != 3 {
		t.Fatalf("done item %v", item)
	}
	if first, last := artifacts[0].(map[string]any), artifacts[2].(map[string]any); first["type"] != "e" || first["value"] != root.ID || last["type"] != "r" || last["value"] != "https://example.com/preview" {
		t.Fatalf("artifacts %v", artifacts)
	}
	item = approvalByID(items, failed.ID)
	failure, _ := item["error"].(map[string]any)
	if item["state"] != "failed" || item["status"] != "failed" || item["result"] != nil || failure == nil || failure["content"] != "no such input" {
		t.Fatalf("failed item %v", item)
	}
	item = approvalByID(items, cancelled.ID)
	if item["state"] != "cancelled" || item["status"] != "cancelled" || item["cancelled_at"] != float64(now-260) || item["progress"].(map[string]any)["content"] != "still going" {
		t.Fatalf("cancelled item %v", item)
	}

	for state, want := range map[string][]string{"open": {open.ID}, "done": {done.ID}, "failed": {failed.ID}, "cancelled": {cancelled.ID}} {
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
	if items := wikiItems(mine, "items"); len(items) != 3 || approvalByID(items, failed.ID) != nil {
		t.Fatalf("mine %v", items)
	}
	if _, err := approvalCall(t, tenant, member, "browsejobs", map[string]any{"state": "bogus"}); err == nil || !strings.HasPrefix(err.Error(), "invalid:") {
		t.Fatalf("bogus state %v", err)
	}
	if _, err := approvalCall(t, tenant, "", "browsejobs", map[string]any{"mine": true}); err == nil || !strings.HasPrefix(err.Error(), "auth-required:") {
		t.Fatalf("guest mine %v", err)
	}
	// Guests see the requests on an open relay; pages follow the cursor.
	paged, err := approvalCall(t, tenant, "", "browsejobs", map[string]any{"limit": 3})
	if err != nil {
		t.Fatal(err)
	}
	if items := wikiItems(paged, "items"); len(items) != 3 || paged["next_cursor"] == "" {
		t.Fatalf("paged %v", paged)
	}
	rest, err := approvalCall(t, tenant, "", "browsejobs", map[string]any{"limit": 3, "cursor": paged["next_cursor"]})
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
	timeline := wikiItems(detail, "answers")
	if item := detail["item"].(map[string]any); item["state"] != "open" || len(timeline) != 2 || timeline[0]["kind"] != float64(event.KIND_JOB_ACCEPTED) || timeline[1]["id"] != working.ID {
		t.Fatalf("open detail %v", detail)
	}
	detail, err = approvalCall(t, tenant, member, "browsejob", map[string]any{"id": cancelled.ID})
	if err != nil {
		t.Fatal(err)
	}
	if timeline := wikiItems(detail, "answers"); len(timeline) != 3 || timeline[1]["kind"] != float64(event.KIND_JOB_CANCEL) || timeline[1]["provider"] != member {
		t.Fatalf("cancelled detail %v", detail)
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

func TestMCPJobBuildersKeepRoomScope(t *testing.T) {
	room := "private-work"
	agent := strings.Repeat("b", 64)
	request, err := mcpBuildJobRequest(mcp.Call{Arguments: map[string]any{"room": room, "assignees": []any{agent}, "content": "hello"}})
	if err != nil {
		t.Fatalf("request builder: %v", err)
	}
	if got := event.Tag(event.Event{Tags: request.Tags}, "h"); got != room {
		t.Fatalf("request room = %q, want %q", got, room)
	}
	requestEvent := map[string]any{"id": strings.Repeat("c", 64), "pubkey": strings.Repeat("d", 64), "kind": float64(event.KIND_JOB_REQUEST), "created_at": float64(1), "tags": request.Tags, "content": "hello"}
	for name, build := range map[string]func(mcp.Call) (mcpUnsigned, error){"accept": mcpBuildJobAnswer(event.KIND_JOB_ACCEPTED), "result": mcpBuildJobAnswer(event.KIND_JOB_RESULT), "cancel": mcpBuildJobCancel} {
		answer, err := build(mcp.Call{Arguments: map[string]any{"request": requestEvent, "content": "done"}})
		if err != nil {
			t.Fatalf("%s builder: %v", name, err)
		}
		if got := event.Tag(event.Event{Tags: answer.Tags}, "h"); got != room {
			t.Fatalf("%s room = %q, want %q", name, got, room)
		}
		if got := event.Tag(event.Event{Tags: answer.Tags}, "e"); got != strings.Repeat("c", 64) {
			t.Fatalf("%s request = %q", name, got)
		}
	}
	if _, err := mcpBuildJobAnswer(event.KIND_JOB_PROGRESS)(mcp.Call{Arguments: map[string]any{"e": strings.Repeat("c", 64), "p": strings.Repeat("d", 64), "content": "x"}}); err == nil {
		t.Fatal("progress builder accepted an answer without a room")
	}
}

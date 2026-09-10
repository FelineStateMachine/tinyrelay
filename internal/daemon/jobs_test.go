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

func TestMCPJobToolsBuildValidateAndPublish(t *testing.T) {
	app, _, member, _ := jobTenant(t)
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
	_, response := mcpCall{method: "tools/list", sign: true}.do(t, app, "/mcp")
	names := map[string]bool{}
	for _, tool := range response.Result.(map[string]any)["tools"].([]any) {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"list_jobs", "read_job", "request_job", "job_feedback", "job_result"} {
		if !names[want] {
			t.Fatalf("missing tool %s", want)
		}
	}

	// request_job: the template carries the NIP-90 tags in order, refuses
	// bad inputs and kinds, and the signed request publishes.
	source := strings.Repeat("d", 64)
	expires := strconv.FormatInt(now+3600, 10)
	result, isError := call("request_job", map[string]any{"kind": 5001, "inputs": []any{map[string]any{"data": "hello", "type": "text"}, map[string]any{"data": source, "type": "event", "relay": "wss://relay.example", "marker": "context"}}, "output": "text/plain", "params": map[string]any{"model": "small", "lang": "es"}, "bid": 1000, "relays": []any{"wss://a.example", " "}, "expiration": now + 3600}, testMemberSecret)
	unsigned, _ := structured(result)["unsigned"].(map[string]any)
	tags, _ := json.Marshal(unsigned["tags"])
	if isError || unsigned["kind"] != float64(5001) || unsigned["content"] != "" || string(tags) != `[["i","hello","text"],["i","`+source+`","event","wss://relay.example","context"],["output","text/plain"],["param","lang","es"],["param","model","small"],["bid","1000"],["relays","wss://a.example"],["expiration","`+expires+`"]]` {
		t.Fatalf("request_job template: %v %s", isError, text(result))
	}
	if result, isError = call("request_job", map[string]any{"kind": 5001, "inputs": []any{map[string]any{"data": "hello", "type": "voice"}}}, testMemberSecret); !isError || !strings.Contains(text(result), "url, event, job") {
		t.Fatalf("request_job bad input: %s", text(result))
	}
	if result, isError = call("request_job", map[string]any{"kind": 1}, testMemberSecret); !isError {
		t.Fatalf("request_job bad kind: %s", text(result))
	}
	if result, isError = call("request_job", map[string]any{"event": mcpSigned(t, testMemberSecret, 5001, [][]string{{"i", "hello"}}, "")}, testMemberSecret); !isError || !strings.Contains(text(result), "url, event, job or text") || !strings.Contains(text(result), "Expected a signed event of kind 5000 to 5127 or 5129 to 5999") {
		t.Fatalf("request_job malformed signed: %s", text(result))
	}
	request := mcpSigned(t, testMemberSecret, 5001, [][]string{{"i", "hello", "text"}, {"output", "text/plain"}, {"bid", "1000"}}, "")
	result, isError = call("request_job", map[string]any{"event": request}, testMemberSecret)
	requestID, _ := structured(result)["event_id"].(string)
	if isError || structured(result)["accepted"] != true || requestID == "" {
		t.Fatalf("request_job publish: %s", text(result))
	}

	// job_feedback: the serving agent's template and signed publish, and
	// the status vocabulary.
	result, isError = call("job_feedback", map[string]any{"e": requestID, "p": member, "status": "processing", "info": "halfway", "amount": 500, "invoice": "lnbc1"}, testAgentSecret)
	unsigned, _ = structured(result)["unsigned"].(map[string]any)
	tags, _ = json.Marshal(unsigned["tags"])
	if isError || unsigned["kind"] != float64(7000) || string(tags) != `[["status","processing","halfway"],["e","`+requestID+`"],["p","`+member+`"],["amount","500","lnbc1"]]` {
		t.Fatalf("job_feedback template: %v %s", isError, text(result))
	}
	if result, isError = call("job_feedback", map[string]any{"e": requestID, "p": member, "status": "done"}, testAgentSecret); !isError || !strings.Contains(text(result), "status") {
		t.Fatalf("job_feedback bad status: %s", text(result))
	}
	if result, isError = call("job_feedback", map[string]any{"event": mcpSigned(t, testAgentSecret, 7000, [][]string{{"e", requestID}, {"p", member}}, "")}, testAgentSecret); !isError || !strings.Contains(text(result), "payment-required, processing, error, success or partial") {
		t.Fatalf("job_feedback malformed signed: %s", text(result))
	}
	result, isError = call("job_feedback", map[string]any{"event": mcpSigned(t, testAgentSecret, 7000, [][]string{{"status", "processing", "halfway"}, {"e", requestID}, {"p", member}}, "")}, testAgentSecret)
	if isError || structured(result)["accepted"] != true {
		t.Fatalf("job_feedback publish: %s", text(result))
	}

	// job_result: the template derives the kind, the request tag and the
	// inputs from the request; the relay checks the answer against the
	// request it holds.
	result, isError = call("job_result", map[string]any{"request": request, "content": "hola", "amount": 700}, testAgentSecret)
	unsigned, _ = structured(result)["unsigned"].(map[string]any)
	resultTags, _ := unsigned["tags"].([]any)
	if isError || unsigned["kind"] != float64(6001) || unsigned["content"] != "hola" || len(resultTags) != 5 {
		t.Fatalf("job_result template: %v %s", isError, text(result))
	}
	requestTag := resultTags[0].([]any)
	var embedded event.Event
	if requestTag[0] != "request" || json.Unmarshal([]byte(requestTag[1].(string)), &embedded) != nil || embedded.ID != requestID {
		t.Fatalf("job_result request tag %v", requestTag)
	}
	tags, _ = json.Marshal(resultTags[1:])
	if string(tags) != `[["e","`+requestID+`"],["i","hello","text"],["p","`+member+`"],["amount","700"]]` {
		t.Fatalf("job_result tags %s", tags)
	}
	if result, isError = call("job_result", map[string]any{"request": request, "kind": 6002, "content": "hola"}, testAgentSecret); !isError || !strings.Contains(text(result), "must match the request") {
		t.Fatalf("job_result mismatched kind: %s", text(result))
	}
	if result, isError = call("job_result", map[string]any{"kind": 6001, "e": requestID, "p": member}, testAgentSecret); !isError || !strings.Contains(text(result), "content is required") {
		t.Fatalf("job_result without content: %s", text(result))
	}
	if result, isError = call("job_result", map[string]any{"event": mcpSigned(t, testAgentSecret, 6002, [][]string{{"e", requestID}, {"p", member}}, "hola")}, testAgentSecret); !isError || !strings.Contains(text(result), "does not answer a kind 5001 request") {
		t.Fatalf("job_result wrong kind rejected by relay: %s", text(result))
	}
	signedTags := [][]string{}
	for _, tag := range resultTags {
		values := []string{}
		for _, value := range tag.([]any) {
			values = append(values, value.(string))
		}
		signedTags = append(signedTags, values)
	}
	result, isError = call("job_result", map[string]any{"event": mcpSigned(t, testAgentSecret, 6001, signedTags, "hola")}, testAgentSecret)
	resultID, _ := structured(result)["event_id"].(string)
	if isError || structured(result)["accepted"] != true || resultID == "" {
		t.Fatalf("job_result publish: %s", text(result))
	}

	// The reads show the settled request.
	result, isError = call("list_jobs", map[string]any{"state": "done", "mine": true}, testMemberSecret)
	items, _ := structured(result)["items"].([]any)
	if isError || len(items) != 1 || items[0].(map[string]any)["id"] != requestID || items[0].(map[string]any)["status"] != "processing" || items[0].(map[string]any)["result"].(map[string]any)["id"] != resultID {
		t.Fatalf("list_jobs: %s", text(result))
	}
	result, isError = call("read_job", map[string]any{"id": requestID}, testMemberSecret)
	if isError || len(structured(result)["feedback"].([]any)) != 1 || len(structured(result)["results"].([]any)) != 1 {
		t.Fatalf("read_job: %s", text(result))
	}
	r := httptest.NewRequest(http.MethodGet, "http://relay.test/llms.txt", nil)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "request_job, job_feedback and job_result") || !strings.Contains(w.Body.String(), "list_jobs and read_job") {
		t.Fatalf("llms.txt: %s", w.Body.String())
	}
}

func TestJobAnswersWakeTheRequesterAsMentions(t *testing.T) {
	_, tenant, member, agent := jobTenant(t)
	ctx := context.Background()
	now := time.Now().Unix()
	request := wikiPublish(t, tenant, testMemberSecret, 5001, now-100, "", []string{"i", "hello", "text"})
	result := event.Event{Kind: 6001, PubKey: agent, CreatedAt: now, Tags: [][]string{{"e", request.ID}, {"p", member}}, Content: "hola"}
	notices := tenant.pushNotices(ctx, result)
	if len(notices) != 1 || notices[0].category != pushMentions || notices[0].recipient != member || notices[0].body != "job 5001 result" || !strings.HasSuffix(notices[0].url, "/e/"+result.ID) || len(notices[0].actions) != 0 {
		t.Fatalf("result notices %+v", notices)
	}
	for status, want := range map[string]int{"error": 1, "payment-required": 1, "processing": 0, "success": 0, "partial": 0} {
		feedback := event.Event{Kind: 7000, PubKey: agent, CreatedAt: now, Tags: [][]string{{"status", status}, {"e", request.ID}, {"p", member}}}
		notices := tenant.pushNotices(ctx, feedback)
		if len(notices) != want {
			t.Fatalf("%s feedback notices %+v", status, notices)
		}
		if want == 1 && (notices[0].category != pushMentions || notices[0].body != "job 5001 "+status) {
			t.Fatalf("%s feedback notice %+v", status, notices[0])
		}
	}
	// Feedback for a request the relay does not hold names its own kind.
	elsewhere := event.Event{Kind: 7000, PubKey: agent, CreatedAt: now, Tags: [][]string{{"status", "error"}, {"e", strings.Repeat("e", 64)}, {"p", member}}}
	if notices := tenant.pushNotices(ctx, elsewhere); len(notices) != 1 || notices[0].body != "job 7000 error" {
		t.Fatalf("elsewhere notices %+v", notices)
	}
	// The requester's own feedback wakes nobody.
	own := event.Event{Kind: 7000, PubKey: member, CreatedAt: now, Tags: [][]string{{"status", "error"}, {"e", request.ID}, {"p", member}}}
	if notices := tenant.pushNotices(ctx, own); len(notices) != 0 {
		t.Fatalf("own notices %+v", notices)
	}
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

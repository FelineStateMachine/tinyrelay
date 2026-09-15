package tinyclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

type approvalsBackend struct {
	*fakeBackend
	calls []string
}

func (b *approvalsBackend) Query(_ context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	b.calls = append(b.calls, method)
	b.params = params
	if actor == "" {
		return nil, errors.New("auth-required: sign in to see requests addressed to you")
	}
	asker := strings.Repeat("c", 64)
	open := map[string]any{"id": strings.Repeat("1", 64), "kind": 1111, "type": "approve", "asker": asker, "subject": "Publish release notes 1.4", "content": "Publish release notes 1.4 to the articles feed as drafted?", "created_at": 1788876000, "expires": 1788897600, "state": "open",
		"about": map[string]any{"coordinate": "30617:" + b.policy.Owner + ":tinyrelay", "event": strings.Repeat("d", 64), "kind": "1621", "url": "http://relay.example/repos/" + b.policy.Owner + "/tinyrelay/issues/" + strings.Repeat("d", 64)}}
	question := map[string]any{"id": strings.Repeat("2", 64), "kind": 9, "type": "question", "asker": asker, "content": "Mention the edge?", "created_at": 1788876100, "state": "open", "room": "build"}
	answered := map[string]any{"id": strings.Repeat("3", 64), "kind": 1111, "type": "decide", "asker": asker, "subject": "Close issue 41 as duplicate of 12", "content": "", "created_at": 1788870000, "state": "answered", "answer": map[string]any{"id": strings.Repeat("e", 64), "kind": 7, "author": b.policy.Owner, "decision": "approved", "content": "+", "created_at": 1788871000}}
	expired := map[string]any{"id": strings.Repeat("4", 64), "kind": 1111, "type": "approve", "asker": asker, "content": "Create room #release", "created_at": 1788860000, "expires": 1788863600, "state": "expired"}
	// Tiny agent interactions: a multiple-choice question with a free-text
	// answer, an open-ended text question, a single-choice approval, and an
	// answered question whose combined answer is shown by option label.
	native := []any{map[string]any{"id": "ship", "label": "Ship it"}, map[string]any{"id": "hold", "label": "Hold"}}
	nativeEvent := map[string]any{"tags": []any{[]any{"tinyagent", "1"}, []any{"h", "build"}}}
	nativeQuestion := map[string]any{"id": strings.Repeat("6", 64), "kind": 9, "type": "question", "asker": asker, "asked": []string{b.policy.Owner}, "subject": "Ship the build?", "content": "The build is green.", "created_at": 1788876200, "expires": 1788897600, "state": "open", "room": "build", "interaction": "question", "selection": "multiple", "freeform": true, "options": native, "event": nativeEvent}
	nativeText := map[string]any{"id": strings.Repeat("7", 64), "kind": 9, "type": "question", "asker": asker, "asked": []string{b.policy.Owner}, "content": "Which branch?", "created_at": 1788876300, "state": "open", "room": "build", "interaction": "question", "selection": "text", "event": nativeEvent}
	nativeApproval := map[string]any{"id": strings.Repeat("8", 64), "kind": 9, "type": "approve", "asker": asker, "asked": []string{b.policy.Owner}, "content": "Delete the stale branch?", "created_at": 1788876400, "state": "open", "room": "build", "interaction": "approval", "selection": "single", "options": []any{map[string]any{"id": "yes", "label": "Yes"}, map[string]any{"id": "no", "label": "No"}}, "event": nativeEvent}
	nativeAnswered := map[string]any{"id": strings.Repeat("9", 64), "kind": 9, "type": "question", "asker": asker, "asked": []string{b.policy.Owner}, "subject": "Ship 1.3?", "created_at": 1788869000, "state": "answered", "room": "build", "interaction": "question", "selection": "multiple", "freeform": true, "options": native, "event": nativeEvent, "answer": map[string]any{"id": strings.Repeat("f", 64), "kind": 1111, "author": b.policy.Owner, "decision": "replied", "content": `{"choices":["ship"],"text":"Go."}`, "created_at": 1788869500}}
	switch method {
	case "browseapprovals":
		return map[string]any{"items": []any{open, question, answered, expired, nativeQuestion, nativeText, nativeApproval, nativeAnswered}, "next_cursor": "", "counts": map[string]int{"open": 5, "answered": 2, "expired": 1}, "oldest_open": 1788876000, "devices": []any{map[string]any{"created_at": 1788800000, "categories": []string{"approvals", "mentions"}}}}, nil
	case "browseapproval":
		var q map[string]any
		_ = json.Unmarshal(params[0], &q)
		if q["id"] != strings.Repeat("5", 64) {
			return nil, errors.New("not found: request")
		}
		return map[string]any{"item": map[string]any{"id": strings.Repeat("5", 64), "kind": 11, "type": "decide", "asker": asker, "subject": "Older request", "content": "Body", "created_at": 1788000000, "state": "open"}, "answers": []any{}}, nil
	}
	return map[string]any{}, nil
}

func TestApprovalsPageRendersRequestsForTheOwner(t *testing.T) {
	backend := &approvalsBackend{fakeBackend: &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return backend.policy.Owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/approvals", nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, body)
	}
	var params map[string]any
	if err := json.Unmarshal(backend.params[0], &params); err != nil || params["state"] != "all" || params["limit"] != 50.0 {
		t.Fatalf("browse params %s %v", backend.params[0], err)
	}
	asker := strings.Repeat("c", 64)
	for _, marker := range []string{
		`<approval-item id="approval-` + strings.Repeat("1", 64) + `" data-type="approve" data-state="open">`,
		`<nostr-name pubkey="` + asker + `"`, "in repository tinyrelay", "expires <time",
		"<p>Publish release notes 1.4</p>", "<p>Publish release notes 1.4 to the articles feed as drafted?</p>",
		"<pre>about 30617:" + backend.policy.Owner + ":tinyrelay | kind 1621 dddddddddddd</pre>",
		`<nostr-react event="` + strings.Repeat("1", 64) + `" pubkey="` + asker + `" kind="1111"><button name="reaction" value="+" data-primary>Approve</button> <button name="reaction" value="-" data-danger>Deny</button></nostr-react>`,
		`<nostr-compose kind="1111" root="` + strings.Repeat("1", 64) + `" root-pubkey="` + asker + `" root-kind="1111" coordinate="30617:` + backend.policy.Owner + `:tinyrelay">`,
		`<a href="/repos/` + backend.policy.Owner + `/tinyrelay/issues/` + strings.Repeat("d", 64) + `">open</a>`,
		`<approval-item id="approval-` + strings.Repeat("2", 64) + `" data-type="question"`, "in #build", `<a href="/e/` + strings.Repeat("2", 64) + `">open</a>`,
		"<h3>Answered</h3>", `<tr id="approval-` + strings.Repeat("3", 64) + `" data-state="answered">`, `<status-badge kind="approved">approved</status-badge> <time`, `data-state="expired"><td>`, `<td><status-badge kind="expired">expired</status-badge></td>`,
		"<h4>Waiting</h4>", "<th>open</th><td>5</td>", "<th>oldest</th><td><time", "<h4>Devices</h4>", "<p><i></i>device 1 | approvals, mentions</p>", "<p><i></i>device 1 | ",
		`<a href="/approvals" aria-current="page">/approvals</a>`,
		// A native interaction renders the room card's choices in a
		// chat-decision whose host re-reads this page, and shows an answer
		// by option label.
		`<chat-activity endpoint="/approvals" room="build" actor="` + backend.policy.Owner + `"><chat-decision event="` + strings.Repeat("6", 64) + `" pubkey="` + asker + `" kind="9" room="build" actor="` + backend.policy.Owner + `" expires="1788897600" interaction="question" selection="multiple" native freeform question multiple>`,
		`<legend>Choose one or more options</legend>`, `<label><input type="checkbox" name="option" value="ship"> Ship it</label><label><input type="checkbox" name="option" value="hold"> Hold</label></fieldset><label>Add details or write your own answer <textarea name="custom-answer" maxlength="8000" rows="2"></textarea></label><button type="submit" data-primary>Submit</button></chat-decision><noscript><p>Answering needs JavaScript and a connected signer.</p></noscript></chat-activity>`,
		`<chat-decision event="` + strings.Repeat("7", 64) + `" pubkey="` + asker + `" kind="9" room="build" actor="` + backend.policy.Owner + `" expires="0" interaction="question" selection="text" native freeform question><label>Your answer <textarea name="custom-answer"`,
		`<chat-decision event="` + strings.Repeat("8", 64) + `" pubkey="` + asker + `" kind="9" room="build" actor="` + backend.policy.Owner + `" expires="0" interaction="approval" selection="single" native><fieldset><legend>Choose an option</legend><label><input type="radio" name="option" value="yes"> Yes</label><label><input type="radio" name="option" value="no"> No</label></fieldset><button type="submit" data-primary>Submit</button></chat-decision>`,
		`<tr id="approval-` + strings.Repeat("9", 64) + `" data-state="answered">`, "<small>Ship it\n\nGo.</small>",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("missing %q in\n%s", marker, body)
		}
	}
	// The question type offers no decision buttons, only a reply.
	question := body[strings.Index(body, `id="approval-`+strings.Repeat("2", 64)):]
	question = question[:strings.Index(question, "</approval-item>")]
	if strings.Contains(question, "<nostr-react") || !strings.Contains(question, `<nostr-compose kind="1111" root="`+strings.Repeat("2", 64)+`" root-pubkey="`+asker+`" root-kind="9">`) {
		t.Fatalf("question controls:\n%s", question)
	}
	// A native item offers neither the reaction buttons nor the plain reply,
	// which could not carry a valid answer; its host must not poll or swap.
	for _, id := range []string{strings.Repeat("6", 64), strings.Repeat("7", 64), strings.Repeat("8", 64)} {
		item := body[strings.Index(body, `id="approval-`+id):]
		item = item[:strings.Index(item, "</approval-item>")]
		if strings.Contains(item, "<nostr-react") || strings.Contains(item, "<nostr-compose") || strings.Contains(item, "fx-action") || strings.Contains(item, "fx-trigger") || !strings.Contains(item, `<footer><a href="/e/`+id+`">open</a></footer>`) {
			t.Fatalf("native controls for %s:\n%s", id[:4], item)
		}
	}
	approval := body[strings.Index(body, `id="approval-`+strings.Repeat("8", 64)):]
	approval = approval[:strings.Index(approval, "</approval-item>")]
	if strings.Contains(approval, "<textarea") {
		t.Fatalf("native approval offers free text:\n%s", approval)
	}
	if strings.Contains(body, "class=") {
		t.Fatal("page carries class attributes")
	}
	rail := body[strings.Index(body, `id="railbox"`):strings.Index(body, `id="rail-tools"`)]
	if strings.Contains(rail, "/inbox") || strings.Contains(rail, "/outbox") || strings.Index(rail, "/approvals") < strings.Index(rail, "/repos") {
		t.Fatalf("rail order:\n%s", rail)
	}

	// A notification opens one request; when it has left the first page it
	// is read on its own and shown first.
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/approvals/"+strings.Repeat("5", 64)+"?answer=approve", nil))
	body = recorder.Body.String()
	if !strings.Contains(strings.Join(backend.calls, ","), "browseapprovals,browseapproval") {
		t.Fatalf("calls %v", backend.calls)
	}
	first := strings.Index(body, `id="approval-`+strings.Repeat("5", 64)+`"`)
	if first < 0 || first > strings.Index(body, `id="approval-`+strings.Repeat("1", 64)+`"`) || !strings.Contains(body, "<p>Older request</p><p>Body</p>") {
		t.Fatalf("focused request missing or not first:\n%s", body)
	}
}

func TestGrantScopeRowsRenderNumericKinds(t *testing.T) {
	review := map[string]any{
		"before": map[string]any{"agent": strings.Repeat("a", 64), "scope": map[string]any{"kinds": []any{9.0, 30617.0}}},
		"after":  map[string]any{"agent": strings.Repeat("a", 64), "scope": map[string]any{"kinds": []any{9.0, 30617.0, 30618.0}}},
	}
	rows := grantScopeRows(review)
	if got := rows[7].Before; got != "9, 30617" {
		t.Fatalf("before kinds = %q", got)
	}
	if got := rows[7].After; got != "9, 30617, 30618" {
		t.Fatalf("after kinds = %q", got)
	}
}

func TestApprovalsPageAsksGuestsToSignIn(t *testing.T) {
	backend := &approvalsBackend{fakeBackend: &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/approvals", nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, `<a href="/signin?next=%2Fapprovals">Sign in</a> to see the requests addressed to your key.`) {
		t.Fatalf("guest page %d:\n%s", recorder.Code, body)
	}
	if strings.Contains(body, "<approval-item") || strings.Contains(body, "auth-required") || strings.Contains(body, "role=\"alert\"") {
		t.Fatalf("guest page leaks requests or the error:\n%s", body)
	}
	if !strings.Contains(body, "Sign in to see which devices are notified.") || strings.Contains(body, "<h4>Waiting</h4>") {
		t.Fatalf("guest panel:\n%s", body)
	}
}

func TestApprovalViewReadsNativeInteractionFields(t *testing.T) {
	asker := strings.Repeat("c", 64)
	tagged := map[string]any{"tags": []any{[]any{"tinyagent", "1"}}}
	options := []any{map[string]any{"id": "ship", "label": "Ship it"}, map[string]any{"id": "hold", "label": "Hold"}}
	multiple := approvalViewFrom(map[string]any{"id": strings.Repeat("6", 64), "kind": 9, "type": "question", "asker": asker, "room": "build", "state": "answered", "interaction": "question", "selection": "multiple", "freeform": true, "options": options, "event": tagged, "answer": map[string]any{"decision": "replied", "content": `["ship","hold"]`}}, "")
	if !multiple.Native || multiple.Interaction != "question" || multiple.Selection != "multiple" || !multiple.Multiple || !multiple.Freeform || !multiple.Question || multiple.Room != "build" || len(multiple.Options) != 2 || multiple.Options[1] != (chatActivityOption{ID: "hold", Label: "Hold"}) || multiple.AnswerContent != "Ship it\nHold" {
		t.Fatalf("multiple-choice view %+v", multiple)
	}
	text := approvalViewFrom(map[string]any{"id": strings.Repeat("7", 64), "kind": 9, "type": "question", "asker": asker, "room": "build", "state": "open", "interaction": "question", "selection": "text", "event": tagged}, "")
	if !text.Native || text.Selection != "text" || text.Multiple || !text.Freeform || !text.Question || len(text.Options) != 0 {
		t.Fatalf("text view %+v", text)
	}
	approval := approvalViewFrom(map[string]any{"id": strings.Repeat("8", 64), "kind": 9, "type": "approve", "asker": asker, "room": "build", "state": "open", "interaction": "approval", "selection": "single", "options": options, "event": tagged}, "")
	if !approval.Native || approval.Selection != "single" || approval.Multiple || approval.Freeform || approval.Question || approval.Interaction != "approval" {
		t.Fatalf("approval view %+v", approval)
	}
	// Without options and without the tinyagent tag a selection is not a
	// native interaction; an ordinary question keeps Question for its type.
	plain := approvalViewFrom(map[string]any{"id": strings.Repeat("2", 64), "kind": 9, "type": "question", "asker": asker, "room": "build", "state": "open", "selection": "single", "event": map[string]any{"tags": []any{}}}, "")
	if plain.Native || !plain.Question || plain.Selection != "" || plain.Interaction != "" || plain.Options != nil {
		t.Fatalf("plain view %+v", plain)
	}
	ordinary := approvalViewFrom(map[string]any{"id": strings.Repeat("1", 64), "kind": 1111, "type": "approve", "asker": asker, "state": "answered", "answer": map[string]any{"decision": "replied", "content": `["ship"]`}}, "")
	if ordinary.Native || ordinary.Question || ordinary.Room != "" || ordinary.AnswerContent != `["ship"]` {
		t.Fatalf("ordinary view %+v", ordinary)
	}
}

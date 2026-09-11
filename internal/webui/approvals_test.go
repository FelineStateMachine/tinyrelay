package webui

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
		"about": map[string]any{"coordinate": "30617:" + b.policy.Owner + ":tinyrelay", "event": strings.Repeat("d", 64), "kind": "1621", "url": "http://relay.example/repo?owner=" + b.policy.Owner + "&repo=tinyrelay&view=issue&id=" + strings.Repeat("d", 64)}}
	question := map[string]any{"id": strings.Repeat("2", 64), "kind": 9, "type": "question", "asker": asker, "content": "Mention the edge?", "created_at": 1788876100, "state": "open", "room": "build"}
	answered := map[string]any{"id": strings.Repeat("3", 64), "kind": 1111, "type": "decide", "asker": asker, "subject": "Close issue 41 as duplicate of 12", "content": "", "created_at": 1788870000, "state": "answered", "answer": map[string]any{"id": strings.Repeat("e", 64), "kind": 7, "author": b.policy.Owner, "decision": "approved", "content": "+", "created_at": 1788871000}}
	expired := map[string]any{"id": strings.Repeat("4", 64), "kind": 1111, "type": "approve", "asker": asker, "content": "Create room #release", "created_at": 1788860000, "expires": 1788863600, "state": "expired"}
	switch method {
	case "browseapprovals":
		return map[string]any{"items": []any{open, question, answered, expired}, "next_cursor": "", "counts": map[string]int{"open": 2, "answered": 1, "expired": 1}, "oldest_open": 1788876000, "devices": []any{map[string]any{"created_at": 1788800000, "categories": []string{"approvals", "mentions"}}}}, nil
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
		`<nostr-react event="` + strings.Repeat("1", 64) + `" pubkey="` + asker + `" kind="1111"><button name="reaction" value="+">Approve</button> <button name="reaction" value="-">Deny</button></nostr-react>`,
		`<nostr-compose kind="1111" root="` + strings.Repeat("1", 64) + `" root-pubkey="` + asker + `" root-kind="1111" coordinate="30617:` + backend.policy.Owner + `:tinyrelay">`,
		`<a href="/repo?owner=` + backend.policy.Owner + `&amp;repo=tinyrelay&amp;view=issue&amp;id=` + strings.Repeat("d", 64) + `">open</a>`,
		`<approval-item id="approval-` + strings.Repeat("2", 64) + `" data-type="question"`, "in #build", `<a href="/e/` + strings.Repeat("2", 64) + `">open</a>`,
		"<h3>Answered</h3>", `<tr id="approval-` + strings.Repeat("3", 64) + `" data-state="answered">`, "approved | <time", `data-state="expired"><td>`, "<td>expired</td>",
		"<h4>Waiting</h4>", "<th>open</th><td>2</td>", "<th>oldest</th><td><time", "<h4>Devices</h4>", "<p><i></i>device 1 | approvals, mentions</p>", "Answers from the phone need the remembered signer.",
		`<a href="/approvals" aria-current="page">/approvals</a>`,
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
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/approvals?id="+strings.Repeat("5", 64)+"&answer=approve", nil))
	body = recorder.Body.String()
	if !strings.Contains(strings.Join(backend.calls, ","), "browseapprovals,browseapproval") {
		t.Fatalf("calls %v", backend.calls)
	}
	first := strings.Index(body, `id="approval-`+strings.Repeat("5", 64)+`"`)
	if first < 0 || first > strings.Index(body, `id="approval-`+strings.Repeat("1", 64)+`"`) || !strings.Contains(body, "<p>Older request</p><p>Body</p>") {
		t.Fatalf("focused request missing or not first:\n%s", body)
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
	if !strings.Contains(body, "Sign in to see which devices are notified.") || !strings.Contains(body, "<th>open</th><td>0</td>") {
		t.Fatalf("guest panel:\n%s", body)
	}
}

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

var activityOpenTask = strings.Repeat("7", 64)

type activityHTTPBackend struct {
	roomsBackend
	seenActor, seenRoom, seenRoot string
	denied                        bool
}

func (b *activityHTTPBackend) ReadChatActivity(_ context.Context, actor, room, root string) (any, error) {
	b.seenActor, b.seenRoom, b.seenRoot = actor, room, root
	if b.denied {
		return nil, errors.New("private details that must stay hidden")
	}
	job := map[string]any{"id": roomThread, "requester": roomOwner, "assignees": []any{roomAgent}, "subject": "Build the preview", "content": "Build it with the dark palette.", "state": "done", "status": "done", "created_at": 1, "result": map[string]any{"id": roomReply, "provider": roomAgent, "content": "Preview is up.", "artifacts": []any{map[string]any{"type": "r", "value": "https://preview.example/site"}, map[string]any{"type": "e", "value": roomReply}}}}
	open := map[string]any{"id": activityOpenTask, "requester": roomOwner, "assignees": []any{roomAgent, roomGuest}, "subject": "Transcribe the call", "state": "open", "status": "running", "created_at": 2, "progress": map[string]any{"id": strings.Repeat("8", 64), "provider": roomAgent, "content": "Halfway through"}}
	return map[string]any{"jobs": []any{job, open}, "approvals": []any{map[string]any{"id": roomReply, "asker": roomAgent, "asked": []string{roomOwner}, "kind": 9, "state": "open", "subject": "Approve <script>bad()</script>", "content": "Review the preview", "type": "question", "interaction": "approval", "selection": "single", "options": []any{map[string]any{"id": "yes", "label": "Yes"}, map[string]any{"id": "no", "label": "No"}}}}}, nil
}

func TestChatActivityEndpointScopesAndEscapesServerRenderedCards(t *testing.T) {
	b := &activityHTTPBackend{roomsBackend: roomsBackend{fakeBackend: fakeBackend{policy: policy.Defaults(roomOwner)}}}
	actor := roomOwner
	app, err := New(b, Options{Actor: func(*http.Request) (string, error) { return actor, nil }})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/chat/activity?room=general&root="+roomThread, nil)
	req.RequestURI = "/r/team/chat/activity?room=general&root=" + roomThread
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)
	body := res.Body.String()
	if res.Code != 200 || res.Header().Get("Cache-Control") != "private, no-store" || b.seenActor != roomOwner || b.seenRoom != "general" || b.seenRoot != roomThread {
		t.Fatalf("request context: %d %v %#v body=%s", res.Code, res.Header(), b, res.Body.String())
	}
	for _, want := range []string{`<chat-decision`, `type="radio"`, `value="yes"`, `interaction="approval"`, `selection="single"`, `<strong>Approval request</strong>`, `href="/r/team/approvals/` + roomReply, `Approve &lt;script&gt;bad()&lt;/script&gt;`, `<noscript>`, `data-activity="job"`, `data-state="done"`, `<strong>Build the preview</strong>`, `data-status="done"`, `<h4>Result</h4>`, `href="https://preview.example/site"`, `href="/r/team/e/` + roomReply + `">Event `, `href="/r/team/e/` + roomThread + `">Open task</a>`,
		`data-status="running">running</small>`, `<p data-progress>Halfway through</p>`, ` for <nostr-name pubkey="` + roomAgent + `"`, `<chat-cancel event="` + activityOpenTask + `" room="general" actor="` + roomOwner + `" assignees="` + roomAgent + ` ` + roomGuest + `"><button type="submit" data-danger>Cancel task</button></chat-cancel>`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "class=") || strings.Contains(body, "<script>bad()") {
		t.Fatal("unsafe markup")
	}
	if strings.Contains(body, "<chat-request") || strings.Contains(body, `id="chat-ask"`) {
		t.Fatal("refresh response carried the request form")
	}
	actor = roomGuest
	res = httptest.NewRecorder()
	app.ServeHTTP(res, req)
	if strings.Contains(res.Body.String(), "chat-decision") || strings.Contains(res.Body.String(), "Review the preview") {
		t.Fatal("approval shown to unaddressed actor")
	}
	if strings.Contains(res.Body.String(), "<chat-cancel") || !strings.Contains(res.Body.String(), `data-event="`+activityOpenTask+`"`) {
		t.Fatal("cancel offered to a viewer who did not request the task")
	}
	b.denied = true
	res = httptest.NewRecorder()
	app.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden || strings.Contains(res.Body.String(), "private details") {
		t.Fatalf("private error leaked: %d %s", res.Code, res.Body.String())
	}
}

func TestChatActivityEndpointRejectsInvalidRoutesAndWrites(t *testing.T) {
	app, _ := roomsApp(t, roomOwner)
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, "/chat/activity?room=general", 405},
		{http.MethodGet, "/chat/activity?room=../private", 400},
		{http.MethodGet, "/chat/activity?room=general&root=bad", 400},
	} {
		t.Run(test.method+test.path, func(t *testing.T) {
			res := httptest.NewRecorder()
			app.ServeHTTP(res, httptest.NewRequest(test.method, test.path, nil))
			if res.Code != test.status {
				t.Fatalf("status %d", res.Code)
			}
		})
	}
}

func TestRoomPageRendersTheRequestFormAndCancelWithoutJavaScript(t *testing.T) {
	b := &activityHTTPBackend{roomsBackend: roomsBackend{fakeBackend: fakeBackend{policy: policy.Defaults(roomOwner)}}}
	actor := roomOwner
	app, err := New(b, Options{Actor: func(*http.Request) (string, error) { return actor, nil }})
	if err != nil {
		t.Fatal(err)
	}
	body := roomsPage(t, app, "/rooms/general")
	form := `<details id="chat-ask"><summary>Ask an agent</summary><chat-request room="general" actor="` + roomOwner + `"><label>Agent <select name="agent" required><option value="` + roomAgent + `">` + roomAgent[:12] + ` (` + roomOwner[:12] + `'s agent)</option></select></label>`
	for _, want := range []string{form, `<input name="subject" maxlength="200"`, `<textarea name="content" required maxlength="8000"`, `<button type="submit" data-primary>Ask</button></chat-request><noscript><p>Publishing needs JavaScript and a connected signer.</p></noscript></details><div id="chat-activity-cards">`, `<chat-cancel event="` + activityOpenTask + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("room page missing %q", want)
		}
	}
	if strings.Contains(body, `<input name="agent"`) {
		t.Error("room with agent members rendered the free key input")
	}
	thread := roomsPage(t, app, "/rooms/general/thread/"+roomThread)
	if !strings.Contains(thread, `<chat-request room="general" actor="`+roomOwner+`" root="`+roomThread+`">`) {
		t.Error("thread page request form is not scoped to the thread")
	}
	actor = ""
	guest := roomsPage(t, app, "/rooms/general")
	if strings.Contains(guest, `id="chat-ask"`) || strings.Contains(guest, "<chat-request") || strings.Contains(guest, "<chat-cancel") {
		t.Error("signed-out room page offered the form or cancel")
	}
}

// A backend whose room read lists no agents falls back to a key input.
type activityNoAgentsBackend struct{ activityHTTPBackend }

func (b *activityNoAgentsBackend) Query(ctx context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	result, err := b.activityHTTPBackend.Query(ctx, method, params, actor)
	if page, ok := result.(map[string]any); ok && method == "browseroom" {
		page["members"] = []any{map[string]any{"pubkey": roomOwner, "role": "owner"}}
	}
	return result, err
}

func TestRoomPageRequestFormAcceptsAKeyWhenNoAgentIsListed(t *testing.T) {
	b := &activityNoAgentsBackend{activityHTTPBackend{roomsBackend: roomsBackend{fakeBackend: fakeBackend{policy: policy.Defaults(roomOwner)}}}}
	app, err := New(b, Options{Actor: func(*http.Request) (string, error) { return roomOwner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	body := roomsPage(t, app, "/rooms/general")
	if !strings.Contains(body, `<label>Agent <input name="agent" required pattern="[0-9a-f]{64}" maxlength="64"`) || strings.Contains(body, `<select name="agent"`) {
		t.Errorf("key input missing: %s", body[strings.Index(body, "chat-ask"):][:400])
	}
}

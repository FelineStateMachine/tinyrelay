package tinyclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

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
	return map[string]any{"jobs": []any{job}, "approvals": []any{map[string]any{"id": roomReply, "asker": roomAgent, "asked": []string{roomOwner}, "kind": 9, "state": "open", "subject": "Approve <script>bad()</script>", "content": "Review the preview", "type": "question", "interaction": "approval", "selection": "single", "options": []any{map[string]any{"id": "yes", "label": "Yes"}, map[string]any{"id": "no", "label": "No"}}}}}, nil
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
	for _, want := range []string{`<chat-decision`, `type="radio"`, `value="yes"`, `interaction="approval"`, `selection="single"`, `<strong>Approval request</strong>`, `href="/r/team/approvals/` + roomReply, `Approve &lt;script&gt;bad()&lt;/script&gt;`, `<noscript>`, `data-activity="job"`, `data-state="done"`, `<strong>Build the preview</strong>`, `data-status="done"`, `<h4>Result</h4>`, `href="https://preview.example/site"`, `href="/r/team/e/` + roomReply + `">Event `, `href="/r/team/e/` + roomThread + `">Open task</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "class=") || strings.Contains(body, "<script>bad()") {
		t.Fatal("unsafe markup")
	}
	actor = roomGuest
	res = httptest.NewRecorder()
	app.ServeHTTP(res, req)
	if strings.Contains(res.Body.String(), "chat-decision") || strings.Contains(res.Body.String(), "Review the preview") {
		t.Fatal("approval shown to unaddressed actor")
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

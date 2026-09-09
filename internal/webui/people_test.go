package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

// peopleBackend answers listjoinrequests with the rows it holds, pending
// first as the relay lists them, and refuses callers other than the owner.
type peopleBackend struct {
	fakeBackend
	requests []any
	calls    []string
}

var (
	askerPending = strings.Repeat("1", 64)
	askerQuiet   = strings.Repeat("2", 64)
	askerDenied  = strings.Repeat("3", 64)
)

func (b *peopleBackend) Query(_ context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	b.calls = append(b.calls, method+":"+actor)
	if method == "listjoinrequests" {
		if actor != b.policy.Owner {
			return nil, context.Canceled
		}
		return b.requests, nil
	}
	return map[string]any{"method": method}, nil
}

func TestPeoplePageListsAccessRequestsWithDecisions(t *testing.T) {
	now := time.Now().Unix()
	owner := strings.Repeat("a", 64)
	backend := &peopleBackend{fakeBackend: fakeBackend{policy: policy.Defaults(owner)}, requests: []any{
		map[string]any{"pubkey": askerPending, "reason": "Met you at the meetup, would like to follow the build.", "requested_at": now - 600, "status": "pending", "decided_by": "", "decided_at": 0},
		map[string]any{"pubkey": askerQuiet, "reason": "", "requested_at": now - 60, "status": "pending", "decided_by": "", "decided_at": 0},
		map[string]any{"pubkey": askerDenied, "reason": "spam", "requested_at": now - 7200, "status": "denied", "decided_by": owner, "decided_at": now - 3600},
	}}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/manage/people", nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, body)
	}
	if strings.Contains(body, "class=") {
		t.Fatal("people page carries a class attribute")
	}
	if strings.Contains(body, "\u00b7") {
		t.Fatal("people page contains a middle dot")
	}
	if backend.calls[0] != "listjoinrequests:"+owner {
		t.Fatalf("requests were not loaded server-side: %v", backend.calls)
	}
	for _, want := range []string{
		`<h3>Access requests</h3>`,
		`<table id="requests"><thead><tr><th>person</th><th>reason</th><th>asked</th><th></th></tr></thead>`,
		`<tr><td><nostr-name pubkey="` + askerPending + `" title="` + askerPending + `">` + shortID(askerPending) + `</nostr-name></td><td>Met you at the meetup, would like to follow the build.</td><td><time datetime="`,
		`<rpc-form method="approvejoin" refresh><input type="hidden" name="param" value="&quot;` + askerPending + `&quot;"><button>Approve</button></rpc-form><rpc-form method="denyjoin" refresh><input type="hidden" name="param" value="&quot;` + askerPending + `&quot;"><button>Deny</button></rpc-form></td></tr>`,
		`<td><small>no reason given</small></td>`,
		`<rpc-form method="approvejoin" refresh><input type="hidden" name="param" value="&quot;` + askerQuiet + `&quot;"><button>Approve</button></rpc-form>`,
		`<ul id="decided"><li data-state="denied"><nostr-name pubkey="` + askerDenied + `"`,
		`<small>denied <time datetime="`,
		` by <nostr-name pubkey="` + owner + `"`,
		`<h4>People</h4><table><tr><th>requests</th><td>2 pending</td></tr></table>`,
		`<h3>Invites</h3>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("people page missing %q", want)
		}
	}
	if strings.Index(body, `<h3>Access requests</h3>`) > strings.Index(body, `<h3>Invites</h3>`) {
		t.Fatal("access requests are not above invites")
	}
	if strings.Contains(body, `value="&quot;`+askerDenied+`&quot;"`) {
		t.Fatal("a decided request offers buttons")
	}
	if strings.Contains(body, "No pending requests.") {
		t.Fatal("empty state shown with pending requests")
	}

	// With nothing pending the section says so and the panel counts zero.
	backend.requests = []any{backend.requests[2]}
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/manage/people", nil))
	body = recorder.Body.String()
	for _, want := range []string{`<h3>Access requests</h3>`, `<p>No pending requests.</p>`, `<ul id="decided">`, `<td>0 pending</td>`} {
		if !strings.Contains(body, want) {
			t.Errorf("empty people page missing %q", want)
		}
	}
	if strings.Contains(body, `<table id="requests">`) {
		t.Fatal("empty people page renders the requests table")
	}

	// A member who may not review sees neither the section nor the count.
	memberApp, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return strings.Repeat("c", 64), nil }})
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	memberApp.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/manage/people", nil))
	body = recorder.Body.String()
	if strings.Contains(body, "Access requests") || strings.Contains(body, "pending</td>") {
		t.Fatal("a member sees the access requests section")
	}
	if !strings.Contains(body, `<h3>Invites</h3>`) {
		t.Fatal("a member lost the rest of the page")
	}
}

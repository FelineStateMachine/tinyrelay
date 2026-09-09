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

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

const (
	testAskerSecret  = "0000000000000000000000000000000000000000000000000000000000000021"
	testSecondSecret = "0000000000000000000000000000000000000000000000000000000000000022"
)

// accessRequestEvent is what Flotilla publishes: a kind 28934 join with no
// claim tag and the reason in the content.
func accessRequestEvent(t *testing.T, secret string, createdAt int64, reason string) event.Event {
	t.Helper()
	return signedEvent(t, secret, event.KIND_NIP43_JOIN, createdAt, [][]string{{"-"}}, reason)
}

func joinRequests(t *testing.T, tenant *Tenant, actor string) []community.JoinRequest {
	t.Helper()
	listed, err := tenant.Execute(context.Background(), actor, "listjoinrequests", nil)
	if err != nil {
		t.Fatal(err)
	}
	return listed.([]community.JoinRequest)
}

// waitProjections lets the tenant worker finish the membership projections
// queued by an approval.
func waitProjections(t *testing.T, tenant *Tenant) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var pending int
		if err := tenant.store.DB().QueryRow(`SELECT count(*) FROM work_intents WHERE kind='records-projection' AND state IN ('pending','running')`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d projections still queued", pending)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func recordsNaming(t *testing.T, tenant *Tenant, kind int, pubkey string) int {
	t.Helper()
	result, err := tenant.store.Query(context.Background(), event.Filter{Kinds: []int{kind}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range result.Events {
		if e.PubKey != tenant.records.PublicKey() {
			continue
		}
		for _, tag := range e.Tags {
			if len(tag) > 1 && tag[1] == pubkey {
				count++
				break
			}
		}
	}
	return count
}

func TestAccessRequestIsHeldReviewedAndWakesReviewersOnce(t *testing.T) {
	app, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	asker, _ := event.PublicKey(testAskerSecret)
	second, _ := event.PublicKey(testSecondSecret)
	member, _ := event.PublicKey(testMemberSecret)
	moderator, _ := event.PublicKey(testModSecret)
	now := time.Now().Unix()

	// The owner's device is registered so a request can wake it.
	service := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) }))
	defer service.Close()
	tenant.pushClient = service.Client()
	body, _ := json.Marshal(testSubscription(t, service.URL+"/send/owner-phone"))
	subscribe := httptest.NewRequest("POST", "http://relay.test/push/subscribe", strings.NewReader(string(body)))
	signRequest(t, subscribe, string(body))
	subscribed := httptest.NewRecorder()
	app.ServeHTTP(subscribed, subscribe)
	if subscribed.Code != 200 {
		t.Fatalf("subscribe %d %s", subscribed.Code, subscribed.Body.String())
	}

	request := accessRequestEvent(t, testAskerSecret, now, "  Met you at the meetup, would like to follow the build.  ")
	reason, err := tenant.Publish(ctx, request, relay.Session{PubKeys: []string{asker}})
	if err != nil {
		t.Fatalf("access request refused: %v", err)
	}
	if reason != "info: access request received, the relay owner will review it" {
		t.Fatalf("OK reason %q", reason)
	}
	rows := joinRequests(t, tenant, owner)
	if len(rows) != 1 || rows[0].PubKey != asker || rows[0].Reason != "Met you at the meetup, would like to follow the build." || rows[0].Status != "pending" || rows[0].RequestedAt == 0 {
		t.Fatalf("listjoinrequests %+v", rows)
	}
	var stored int
	if err := tenant.store.DB().QueryRow(`SELECT count(*) FROM events WHERE kind=?`, event.KIND_NIP43_JOIN).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("request stored as an event: %d %v", stored, err)
	}
	if role, _ := tenant.community.Role(ctx, asker); role != "" {
		t.Fatalf("asker became %q", role)
	}
	notices := tenant.joinRequestNotices(ctx, request)
	if len(notices) != 1 || notices[0].recipient != owner || notices[0].category != pushApprovals || notices[0].body != asker[:12]+" asks to join: Met you at the meetup, would like to follow the build." || notices[0].url != "http://relay.test/manage/people" {
		t.Fatalf("notices %+v", notices)
	}
	if got := countIntents(t, tenant, notificationPush); got != 1 {
		t.Fatalf("device notifications queued = %d, want 1", got)
	}
	// A repeat refreshes the request without waking the owner again.
	if reason, err := tenant.Publish(ctx, accessRequestEvent(t, testAskerSecret, now+1, "Second reason"), relay.Session{PubKeys: []string{asker}}); err != nil || reason != community.JoinRequestReceived {
		t.Fatalf("repeat %q %v", reason, err)
	}
	if rows := joinRequests(t, tenant, owner); len(rows) != 1 || rows[0].Reason != "Second reason" {
		t.Fatalf("refreshed %+v", rows)
	}
	if got := countIntents(t, tenant, notificationPush); got != 1 {
		t.Fatalf("device notifications after repeat = %d, want 1", got)
	}
	// Without a reason the summary says so; moderators are woken too.
	if _, err := tenant.Execute(ctx, owner, "setmember", []json.RawMessage{json.RawMessage(strconv.Quote(moderator)), json.RawMessage(`{"role":"moderator"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.Execute(ctx, owner, "setmember", []json.RawMessage{json.RawMessage(strconv.Quote(member)), json.RawMessage(`{"role":"member"}`)}); err != nil {
		t.Fatal(err)
	}
	quiet := accessRequestEvent(t, testSecondSecret, now, "")
	notices = tenant.joinRequestNotices(ctx, quiet)
	if len(notices) != 2 || notices[0].recipient != owner || notices[1].recipient != moderator || notices[1].body != second[:12]+" asks to join: no reason given" {
		t.Fatalf("moderator notices %+v", notices)
	}
	if _, err := tenant.Publish(ctx, quiet, relay.Session{PubKeys: []string{second}}); err != nil {
		t.Fatal(err)
	}
	// A member asking again is told so, and a claimed join still needs a
	// valid invite.
	if reason, err := tenant.Publish(ctx, accessRequestEvent(t, testMemberSecret, now, "again"), relay.Session{PubKeys: []string{member}}); err != nil || reason != "duplicate: already a member" {
		t.Fatalf("member request %q %v", reason, err)
	}
	claimed := signedEvent(t, testAskerSecret, event.KIND_NIP43_JOIN, now+2, [][]string{{"-"}, {"claim", "nosuchcode"}}, "")
	if _, err := tenant.Publish(ctx, claimed, relay.Session{PubKeys: []string{asker}}); err == nil || err.Error() != "invite_invalid" {
		t.Fatalf("claimed join: %v", err)
	}

	// Only the owner and moderators review.
	methods, err := tenant.Execute(ctx, owner, "supportedmethods", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"listjoinrequests", "approvejoin", "denyjoin"} {
		if !containsString(methods.([]string), name) {
			t.Fatalf("supportedmethods lacks %s", name)
		}
	}
	param := []json.RawMessage{json.RawMessage(strconv.Quote(asker))}
	for _, actor := range []string{member, asker, strings.Repeat("e", 64)} {
		for _, method := range []string{"listjoinrequests", "approvejoin", "denyjoin"} {
			if _, err := tenant.Execute(ctx, actor, method, param); err == nil || !strings.HasPrefix(err.Error(), "restricted:") {
				t.Fatalf("%s by non-admin: %v", method, err)
			}
		}
	}
	if rows := joinRequests(t, tenant, moderator); len(rows) != 2 {
		t.Fatalf("moderator list %+v", rows)
	}

	// Approval makes a member and the signed records follow.
	result, err := tenant.Execute(ctx, moderator, "approvejoin", param)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.(map[string]any); got["status"] != "approved" || got["pubkey"] != asker {
		t.Fatalf("approvejoin = %v", got)
	}
	if role, _ := tenant.community.Role(ctx, asker); role != "member" {
		t.Fatalf("approved role = %q", role)
	}
	waitProjections(t, tenant)
	if n := recordsNaming(t, tenant, event.KIND_ROSTER, asker); n == 0 {
		t.Fatal("member list (kind 13534) does not name the approved key")
	}
	if n := recordsNaming(t, tenant, event.KIND_MEMBER_ADDED, asker); n == 0 {
		t.Fatal("no add-user record (kind 8000) for the approved key")
	}
	rows = joinRequests(t, tenant, owner)
	if len(rows) != 2 || rows[0].PubKey != second || rows[0].Status != "pending" || rows[1].PubKey != asker || rows[1].Status != "approved" || rows[1].DecidedBy != moderator || rows[1].DecidedAt == 0 {
		t.Fatalf("after approval %+v", rows)
	}
	if _, err := tenant.Execute(ctx, owner, "denyjoin", []json.RawMessage{json.RawMessage(strconv.Quote(second))}); err != nil {
		t.Fatal(err)
	}
	byKey := map[string]community.JoinRequest{}
	for _, row := range joinRequests(t, tenant, owner) {
		byKey[row.PubKey] = row
	}
	if len(byKey) != 2 || byKey[second].Status != "denied" || byKey[second].DecidedBy != owner || byKey[asker].Status != "approved" {
		t.Fatalf("after denial %+v", byKey)
	}
	if role, _ := tenant.community.Role(ctx, second); role != "" {
		t.Fatalf("denied role = %q", role)
	}
	audit, _ := tenant.Execute(ctx, owner, "listaudit", nil)
	seen := map[string]bool{}
	for _, row := range audit.([]community.AuditRow) {
		seen[row.Action] = true
	}
	if !seen["approvejoin"] || !seen["denyjoin"] {
		t.Fatalf("audit lacks the decisions: %v", seen)
	}
}

func TestMCPJoinRequestTools(t *testing.T) {
	app, tenant := testTenant(t)
	ctx := context.Background()
	asker, _ := event.PublicKey(testAskerSecret)
	member, _ := event.PublicKey(testMemberSecret)
	if _, err := tenant.Execute(ctx, tenant.Policy().Owner, "setmember", []json.RawMessage{json.RawMessage(strconv.Quote(member)), json.RawMessage(`{"role":"member"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.Publish(ctx, accessRequestEvent(t, testAskerSecret, time.Now().Unix(), "Let me in"), relay.Session{PubKeys: []string{asker}}); err != nil {
		t.Fatal(err)
	}
	_, response := mcpCall{method: "tools/list", sign: true}.do(t, app, "/mcp")
	names := map[string]bool{}
	for _, tool := range response.Result.(map[string]any)["tools"].([]any) {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"list_join_requests", "approve_join", "deny_join"} {
		if !names[want] {
			t.Fatalf("missing tool %s", want)
		}
	}
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
	result, isError := call("list_join_requests", map[string]any{}, "")
	listed, _ := structured(result)["result"].([]any)
	if isError || len(listed) != 1 || listed[0].(map[string]any)["pubkey"] != asker || listed[0].(map[string]any)["reason"] != "Let me in" || listed[0].(map[string]any)["status"] != "pending" {
		t.Fatalf("list_join_requests: %v %v", isError, result)
	}
	if _, isError := call("list_join_requests", map[string]any{}, testMemberSecret); !isError {
		t.Fatal("a member listed access requests")
	}
	if _, isError := call("approve_join", map[string]any{"pubkey": asker}, testMemberSecret); !isError {
		t.Fatal("a member approved an access request")
	}
	result, isError = call("deny_join", map[string]any{"pubkey": asker}, "")
	if isError || structured(result)["result"].(map[string]any)["status"] != "denied" {
		t.Fatalf("deny_join: %v %v", isError, result)
	}
	result, isError = call("approve_join", map[string]any{"pubkey": asker}, "")
	if isError || structured(result)["result"].(map[string]any)["status"] != "approved" {
		t.Fatalf("approve_join: %v %v", isError, result)
	}
	if role, _ := tenant.community.Role(ctx, asker); role != "member" {
		t.Fatalf("role after approve_join = %q", role)
	}
	llms := httptest.NewRecorder()
	app.ServeHTTP(llms, httptest.NewRequest(http.MethodGet, "http://relay.test/llms.txt", nil))
	if body := llms.Body.String(); !strings.Contains(body, "list_join_requests") || !strings.Contains(body, "approve_join, deny_join") {
		t.Fatalf("llms.txt lacks the access request tools: %s", body)
	}
}

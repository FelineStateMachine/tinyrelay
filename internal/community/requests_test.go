package community

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func requestService(t *testing.T) (*Service, *storage.Store, string) {
	t.Helper()
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	owner := strings.Repeat("a", 64)
	svc, err := New(ctx, st, owner)
	if err != nil {
		t.Fatal(err)
	}
	p := policy.Defaults(owner)
	svc.ConfigurePolicy(func() policy.Policy { return p })
	return svc, st, owner
}

func accessRequest(pubkey, content string) event.Event {
	return event.Event{ID: strings.Repeat("1", 64), PubKey: pubkey, Kind: event.KIND_NIP43_JOIN, Tags: [][]string{{"-"}}, Content: content}
}

func joinRequestRow(t *testing.T, st *storage.Store, pubkey string) (JoinRequest, int) {
	t.Helper()
	var r JoinRequest
	var count int
	if err := st.DB().QueryRow(`SELECT count(*) FROM join_requests`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	err := st.DB().QueryRow(`SELECT pubkey,reason,requested_at,status,decided_by,decided_at FROM join_requests WHERE pubkey=?`, pubkey).Scan(&r.PubKey, &r.Reason, &r.RequestedAt, &r.Status, &r.DecidedBy, &r.DecidedAt)
	if err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	return r, count
}

func TestAccessRequestIsRecordedRefreshedAndAnswered(t *testing.T) {
	ctx := context.Background()
	for _, path := range []string{"transactional", "direct"} {
		t.Run(path, func(t *testing.T) {
			svc, st, owner := requestService(t)
			stranger, member, banned := strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)
			persisted := false
			handle := func(ev event.Event) (MembershipResult, error) {
				if path == "direct" {
					return svc.HandleMembershipEvent(ctx, ev)
				}
				return svc.HandleMembershipEventTx(ctx, ev, func(*sql.Tx) error { persisted = true; return nil })
			}
			result, err := handle(accessRequest(stranger, "  Hi, I am Bob from the meetup.  "))
			if err != nil {
				t.Fatal(err)
			}
			if !result.OK || result.Stored || !result.AccessRequest || !result.NewRequest || result.Message != JoinRequestReceived {
				t.Fatalf("first request result %+v", result)
			}
			if result.Message != "info: access request received, the relay owner will review it" {
				t.Fatalf("OK message %q", result.Message)
			}
			row, count := joinRequestRow(t, st, stranger)
			if count != 1 || row.Reason != "Hi, I am Bob from the meetup." || row.Status != "pending" || row.RequestedAt == 0 || row.DecidedBy != "" || row.DecidedAt != 0 {
				t.Fatalf("row %+v count %d", row, count)
			}
			if persisted {
				t.Fatal("access request was stored as an event")
			}
			if role, _ := svc.Role(ctx, stranger); role != "" {
				t.Fatalf("request made a member: %q", role)
			}
			// The owner's own row queued one projection; the request adds none.
			var intents int
			if err := st.DB().QueryRow(`SELECT count(*) FROM work_intents WHERE kind='records-projection'`).Scan(&intents); err != nil || intents != 1 {
				t.Fatalf("request queued a projection: %d %v", intents-1, err)
			}

			// A repeat refreshes the reason and time in the same row and is
			// not a new request.
			if _, err := st.DB().Exec(`UPDATE join_requests SET requested_at=1 WHERE pubkey=?`, stranger); err != nil {
				t.Fatal(err)
			}
			long := strings.Repeat("x", 600)
			result, err = handle(accessRequest(stranger, long))
			if err != nil || !result.OK || result.NewRequest || !result.AccessRequest || result.Message != JoinRequestReceived {
				t.Fatalf("repeat result %+v %v", result, err)
			}
			row, count = joinRequestRow(t, st, stranger)
			if count != 1 || len(row.Reason) != JoinReasonMax || row.RequestedAt == 1 || row.Status != "pending" {
				t.Fatalf("refreshed row %+v count %d", row, count)
			}

			// A member is told so, as before.
			if _, err := svc.Execute(ctx, owner, "setmember", raws(member, map[string]any{"role": "member"})); err != nil {
				t.Fatal(err)
			}
			result, err = handle(accessRequest(member, "again"))
			if err != nil || !result.OK || result.Message != "duplicate: already a member" || result.AccessRequest {
				t.Fatalf("member result %+v %v", result, err)
			}
			if _, count := joinRequestRow(t, st, member); count != 1 {
				t.Fatalf("member request recorded: %d rows", count)
			}

			// A banned key is refused.
			if _, err := svc.Execute(ctx, owner, "banpubkey", raws(banned, "spam")); err != nil {
				t.Fatal(err)
			}
			if _, err := handle(accessRequest(banned, "let me in")); err == nil || !strings.HasPrefix(err.Error(), "restricted:") {
				t.Fatalf("banned request: %v", err)
			}
			if _, count := joinRequestRow(t, st, banned); count != 1 {
				t.Fatalf("banned request recorded: %d rows", count)
			}

			// A claimed join keeps its invite checks.
			claimed := accessRequest(strings.Repeat("e", 64), "")
			claimed.Tags = [][]string{{"-"}, {"claim", "nosuchcode"}}
			if _, err := handle(claimed); err == nil || err.Error() != "invite_invalid" {
				t.Fatalf("claimed join: %v", err)
			}
		})
	}
}

func TestApproveAndDenyAccessRequests(t *testing.T) {
	ctx := context.Background()
	svc, st, owner := requestService(t)
	moderator, member, alice, bob, carol := strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64), strings.Repeat("e", 64), strings.Repeat("f", 64)
	if _, err := svc.Execute(ctx, owner, "setmember", raws(moderator, map[string]any{"role": "moderator"})); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Execute(ctx, owner, "setmember", raws(member, map[string]any{"role": "member"})); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"listjoinrequests", "approvejoin", "denyjoin"} {
		if !containsMethod(svc.Methods(), name) {
			t.Fatalf("methods lack %s", name)
		}
	}
	for _, pk := range []string{alice, bob, carol} {
		if _, err := svc.HandleMembershipEvent(ctx, accessRequest(pk, "please "+pk[:1])); err != nil {
			t.Fatal(err)
		}
	}
	// Only the owner and moderators list or decide.
	for _, actor := range []string{member, alice, strings.Repeat("9", 64)} {
		for _, method := range []string{"listjoinrequests", "approvejoin", "denyjoin"} {
			if _, err := svc.Execute(ctx, actor, method, raws(alice)); err == nil || !strings.HasPrefix(err.Error(), "restricted:") {
				t.Fatalf("%s by %s: %v", method, actor[:1], err)
			}
		}
	}
	listed, err := svc.Execute(ctx, moderator, "listjoinrequests", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rows := listed.([]JoinRequest); len(rows) != 3 || rows[0].Status != "pending" || rows[0].Reason != "please "+rows[0].PubKey[:1] {
		t.Fatalf("listjoinrequests %+v", rows)
	}
	var intentsBefore int
	if err := st.DB().QueryRow(`SELECT count(*) FROM work_intents WHERE kind='records-projection'`).Scan(&intentsBefore); err != nil {
		t.Fatal(err)
	}
	result, err := svc.Execute(ctx, moderator, "approvejoin", raws(alice))
	if err != nil {
		t.Fatal(err)
	}
	if got := result.(map[string]any); got["pubkey"] != alice || got["status"] != "approved" {
		t.Fatalf("approvejoin = %v", got)
	}
	var role, via string
	if err := st.DB().QueryRow(`SELECT role,via FROM community_members WHERE pubkey=?`, alice).Scan(&role, &via); err != nil || role != "member" || via != "request" {
		t.Fatalf("approved member role=%q via=%q err=%v", role, via, err)
	}
	var intentsAfter int
	if err := st.DB().QueryRow(`SELECT count(*) FROM work_intents WHERE kind='records-projection'`).Scan(&intentsAfter); err != nil || intentsAfter != intentsBefore+1 {
		t.Fatalf("approval queued %d projections, want 1", intentsAfter-intentsBefore)
	}
	row, _ := joinRequestRow(t, st, alice)
	if row.Status != "approved" || row.DecidedBy != moderator || row.DecidedAt == 0 {
		t.Fatalf("approved row %+v", row)
	}
	if _, err := svc.Execute(ctx, owner, "denyjoin", raws(alice)); err == nil || !strings.HasPrefix(err.Error(), "invalid:") {
		t.Fatalf("deny after approve: %v", err)
	}
	// A key that already became a member only has its request marked.
	if _, err := svc.Execute(ctx, owner, "setmember", raws(bob, map[string]any{"name": "bob", "role": "member"})); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Execute(ctx, owner, "approvejoin", raws(bob)); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := st.DB().QueryRow(`SELECT name,via FROM community_members WHERE pubkey=?`, bob).Scan(&name, &via); err != nil || name != "bob" || via != "management" {
		t.Fatalf("existing member changed: name=%q via=%q err=%v", name, via, err)
	}
	if row, _ := joinRequestRow(t, st, bob); row.Status != "approved" || row.DecidedBy != owner {
		t.Fatalf("bob row %+v", row)
	}
	if _, err := svc.Execute(ctx, owner, "denyjoin", raws(carol)); err != nil {
		t.Fatal(err)
	}
	if row, _ := joinRequestRow(t, st, carol); row.Status != "denied" || row.DecidedBy != owner || row.DecidedAt == 0 {
		t.Fatalf("carol row %+v", row)
	}
	if role, _ := svc.Role(ctx, carol); role != "" {
		t.Fatalf("denied key became %q", role)
	}
	if _, err := svc.Execute(ctx, owner, "approvejoin", raws(strings.Repeat("8", 64))); err == nil || !strings.HasPrefix(err.Error(), "not found:") {
		t.Fatalf("approve unknown: %v", err)
	}
	// Decided requests follow the pending ones, newest decision first; a
	// denied key may ask again.
	if _, err := st.DB().Exec(`UPDATE join_requests SET decided_at=decided_at-60 WHERE pubkey=?`, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.HandleMembershipEvent(ctx, accessRequest(strings.Repeat("7", 64), "new")); err != nil {
		t.Fatal(err)
	}
	again, err := svc.HandleMembershipEvent(ctx, accessRequest(carol, "second try"))
	if err != nil || !again.NewRequest {
		t.Fatalf("denied key asking again %+v %v", again, err)
	}
	listed, _ = svc.Execute(ctx, owner, "listjoinrequests", nil)
	rows := listed.([]JoinRequest)
	if len(rows) != 4 || rows[0].Status != "pending" || rows[1].Status != "pending" || rows[2].Status == "pending" || rows[3].Status == "pending" {
		t.Fatalf("order %+v", rows)
	}
	if rows[2].PubKey != bob || rows[3].PubKey != alice {
		t.Fatalf("decided order %s %s", rows[2].PubKey[:1], rows[3].PubKey[:1])
	}
	audit, _ := svc.Execute(ctx, owner, "listaudit", nil)
	seen := map[string]bool{}
	for _, row := range audit.([]AuditRow) {
		seen[row.Action+":"+row.Target] = true
	}
	if !seen["approvejoin:"+alice] || !seen["denyjoin:"+carol] || !seen["setmember:"+alice] {
		t.Fatalf("audit %v", seen)
	}
}

func containsMethod(methods []string, wanted string) bool {
	for _, method := range methods {
		if method == wanted {
			return true
		}
	}
	return false
}

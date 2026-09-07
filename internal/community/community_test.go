package community

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestMembershipAuthorizationAndInviteClaim(t *testing.T) {
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	owner, moderator, member, stranger := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)
	svc, err := New(ctx, st, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Execute(ctx, stranger, "setmember", raws(member, "alice", "member")); err == nil {
		t.Fatal("stranger changed membership")
	}
	if _, err := svc.Execute(ctx, owner, "setmember", raws(moderator, map[string]any{"name": "mod", "role": "moderator"})); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Execute(ctx, moderator, "setmember", raws(member, map[string]any{"name": "alice", "role": "member"})); err != nil {
		t.Fatal(err)
	}
	inv, err := svc.Execute(ctx, owner, "createinvite", raws(int64(3600), 1, "test"))
	if err != nil {
		t.Fatal(err)
	}
	code := inv.(Invite).Code
	if _, err := svc.Claim(ctx, code, stranger); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Claim(ctx, code, strings.Repeat("e", 64)); err == nil {
		t.Fatal("single-use claim spent twice")
	}
	if got, err := svc.Role(ctx, stranger); err != nil || got != "member" {
		t.Fatalf("role=%q err=%v", got, err)
	}
	if inv == nil {
		t.Fatal("missing invite result")
	}
}

func TestMemberInvitePolicyTermsAndRevokedParent(t *testing.T) {
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	owner, member, stranger := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	p := policy.Defaults(owner)
	p.MemberInvites = policy.MemberInvites{Depth: 2, Quota: 1}
	p.JoinTerms = "be kind"
	svc, err := New(ctx, st, owner)
	if err != nil {
		t.Fatal(err)
	}
	svc.ConfigurePolicy(func() policy.Policy { return p })
	if _, err := svc.Execute(ctx, owner, "setmember", raws(member, map[string]any{"name": "member", "role": "member"})); err != nil {
		t.Fatal(err)
	}
	inv, err := svc.Execute(ctx, member, "createinvite", raws(int64(3600), 1, "child"))
	if err != nil {
		t.Fatal(err)
	}
	code := inv.(Invite).Code
	if _, err := svc.Execute(ctx, member, "createinvite", raws(int64(3600), 1, "over-quota")); err == nil {
		t.Fatal("member exceeded live invite quota")
	}
	terms := sha256.Sum256([]byte(p.JoinTerms))
	if _, err := svc.ClaimWithTerms(ctx, code, stranger, "wrong"); err == nil {
		t.Fatal("terms mismatch accepted")
	}
	if role, _ := svc.Role(ctx, stranger); role != "" {
		t.Fatalf("terms mismatch wrote membership: %q", role)
	}
	if _, err := svc.Execute(ctx, owner, "removesubtree", raws(member)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClaimWithTerms(ctx, code, stranger, hex.EncodeToString(terms[:])); err == nil {
		t.Fatal("revoked parent invite accepted")
	}
}

func TestIssueNIP43InviteRequiresMembership(t *testing.T) {
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	owner, member, stranger := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	svc, err := New(ctx, st, owner)
	if err != nil {
		t.Fatal(err)
	}
	p := policy.Defaults(owner)
	p.MemberInvites = policy.MemberInvites{Depth: 1, Quota: 1}
	svc.ConfigurePolicy(func() policy.Policy { return p })
	if _, err := svc.IssueNIP43Invite(ctx, stranger); err == nil {
		t.Fatal("stranger received invite")
	}
	if _, err := svc.Execute(ctx, owner, "setmember", raws(member, "member", "member")); err != nil {
		t.Fatal(err)
	}
	invite, err := svc.IssueNIP43Invite(ctx, member)
	if err != nil {
		t.Fatal(err)
	}
	if invite.Code == "" || invite.ExpiresAt <= invite.CreatedAt {
		t.Fatalf("invalid invite: %+v", invite)
	}
}

func TestNIP56PubkeyReportDoesNotHideEventID(t *testing.T) {
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	owner, reporter, target := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	svc, err := New(ctx, st, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SubmitReportTarget(ctx, reporter, target, "pubkey", "spam", "bad actor", 1); err != nil {
		t.Fatal(err)
	}
	var hidden int
	if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM hidden_events WHERE id=?`, target).Scan(&hidden); err != nil {
		t.Fatal(err)
	}
	if hidden != 0 {
		t.Fatal("pubkey report was treated as an event hide")
	}
}

func TestMembershipProjectionIntentRollsBackWithTransaction(t *testing.T) {
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	owner := strings.Repeat("a", 64)
	if _, err := New(ctx, st, owner); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := st.DB().QueryRow(`SELECT count(*) FROM work_intents WHERE kind='records-projection'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	member := strings.Repeat("b", 64)
	if err := st.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO community_members(pubkey,role,created_at) VALUES(?,?,?)`, member, "member", 1); err != nil {
			return err
		}
		return errors.New("force rollback")
	}); err == nil {
		t.Fatal("expected rollback")
	}
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM community_members WHERE pubkey=?`, member).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("membership committed despite rollback")
	}
	if err := st.DB().QueryRow(`SELECT count(*) FROM work_intents WHERE kind='records-projection'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != before {
		t.Fatal("projection intent committed despite rollback")
	}
}

func TestTransactionalMembershipPersistenceRollback(t *testing.T) {
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	owner, stranger := strings.Repeat("a", 64), strings.Repeat("b", 64)
	svc, err := New(ctx, st, owner)
	if err != nil {
		t.Fatal(err)
	}
	p := policy.Defaults(owner)
	svc.ConfigurePolicy(func() policy.Policy { return p })
	_, err = svc.HandleMembershipEventTx(ctx, event.Event{ID: strings.Repeat("c", 64), PubKey: stranger, Kind: event.KIND_JOIN}, func(*sql.Tx) error { return errors.New("event write failed") })
	if err == nil {
		t.Fatal("expected event persistence failure")
	}
	if role, _ := svc.Role(ctx, stranger); role != "" {
		t.Fatal("membership committed after event failure")
	}
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM work_intents WHERE event_id=?`, strings.Repeat("c", 64)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("projection intent committed after event failure")
	}
}

func TestTransferIsAtomicAndDemotesOldOwner(t *testing.T) {
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	owner, member := strings.Repeat("a", 64), strings.Repeat("b", 64)
	svc, err := New(ctx, st, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Execute(ctx, owner, "setmember", raws(member, "new", "member")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Execute(ctx, owner, "transferowner", raws(member)); err != nil {
		t.Fatal(err)
	}
	if role, _ := svc.Role(ctx, owner); role != "moderator" {
		t.Fatalf("old owner role=%q", role)
	}
	if role, _ := svc.Role(ctx, member); role != "owner" {
		t.Fatalf("new owner role=%q", role)
	}
}

func raws(values ...any) []json.RawMessage {
	out := make([]json.RawMessage, len(values))
	for i, value := range values {
		out[i], _ = json.Marshal(value)
	}
	return out
}

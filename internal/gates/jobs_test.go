package gates

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

const jobMemberSecret = "0000000000000000000000000000000000000000000000000000000000000005"

// jobFixture adds a human member who requests jobs to the agent fixture.
type jobFixture struct {
	agentFixture
	member string
}

func newJobFixture(t *testing.T) jobFixture {
	t.Helper()
	f := newAgentFixture(t)
	member, _ := event.PublicKey(jobMemberSecret)
	raw, _ := json.Marshal(member)
	if _, err := f.community.Execute(context.Background(), f.owner, "setmember", []json.RawMessage{raw}); err != nil {
		t.Fatal(err)
	}
	return jobFixture{agentFixture: f, member: member}
}

// request publishes a job request from the member and stores it, so
// answers can name a request the relay holds.
func (f jobFixture) request(t *testing.T, kind int, createdAt int64) event.Event {
	t.Helper()
	ctx := context.Background()
	e := signed(t, jobMemberSecret, kind, createdAt, [][]string{{"i", "hello", "text"}, {"output", "text/plain"}, {"bid", "1000"}}, "")
	if err := f.gate.Write(ctx, e, relay.Session{PubKeys: []string{f.member}}, agentNow); err != nil {
		t.Fatalf("member request rejected: %v", err)
	}
	if err := f.store.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := storage.SaveTx(ctx, tx, e, storage.SaveOptions{Now: agentNow})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

func expectError(t *testing.T, err error, prefix, fragment string) {
	t.Helper()
	if err == nil || !strings.HasPrefix(err.Error(), prefix) || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("want %s error containing %q, got %v", prefix, fragment, err)
	}
}

func TestJobShapeRefusesMalformedEvents(t *testing.T) {
	f := newJobFixture(t)
	ctx := context.Background()
	session := relay.Session{PubKeys: []string{f.member}}
	request := f.request(t, 5001, agentNow)
	cases := []struct {
		name     string
		kind     int
		tags     [][]string
		fragment string
	}{
		{"input without a type", 5001, [][]string{{"i", "hello"}}, "url, event, job or text"},
		{"input with an unknown type", 5001, [][]string{{"i", "hello", "voice"}}, "url, event, job or text"},
		{"event input without an id", 5001, [][]string{{"i", "abc", "event"}}, "event id"},
		{"encrypted without a provider", 5001, [][]string{{"encrypted"}}, "p tag"},
		{"bid that is not an amount", 5001, [][]string{{"bid", "ten"}}, "millisats"},
		{"result without a request", 6001, [][]string{{"p", f.member}}, "e tag"},
		{"result without a requester", 6001, [][]string{{"e", request.ID}}, "p tag"},
		{"result with a bad amount", 6001, [][]string{{"e", request.ID}, {"p", f.member}, {"amount", "-1"}}, "millisats"},
		{"result of the wrong kind", 6002, [][]string{{"e", request.ID}, {"p", f.member}}, "does not answer"},
		{"result naming the wrong requester", 6001, [][]string{{"e", request.ID}, {"p", f.owner}}, "name the requester"},
		{"feedback without a status", 7000, [][]string{{"e", request.ID}, {"p", f.member}}, "payment-required, processing, error, success or partial"},
		{"feedback with an unknown status", 7000, [][]string{{"e", request.ID}, {"p", f.member}, {"status", "done"}}, "payment-required, processing, error, success or partial"},
	}
	for _, tc := range cases {
		err := f.gate.Write(ctx, signed(t, jobMemberSecret, tc.kind, agentNow, tc.tags, ""), session, agentNow)
		expectError(t, err, "invalid:", tc.fragment)
	}
	// Well-formed answers from a member pass, including answers to a request
	// made on another relay.
	for _, e := range []event.Event{
		signed(t, jobMemberSecret, 6001, agentNow, [][]string{{"e", request.ID}, {"p", f.member}, {"amount", "500", "lnbc1"}}, "output"),
		signed(t, jobMemberSecret, 7000, agentNow, [][]string{{"e", request.ID}, {"p", f.member}, {"status", "processing"}}, ""),
		signed(t, jobMemberSecret, 6005, agentNow, [][]string{{"e", strings.Repeat("a", 64)}, {"p", f.owner}}, "elsewhere"),
	} {
		if err := f.gate.Write(ctx, e, session, agentNow); err != nil {
			t.Fatalf("well-formed kind %d rejected: %v", e.Kind, err)
		}
	}
	// Import applies the same shape rules.
	expectError(t, f.gate.Import(ctx, signed(t, jobMemberSecret, 7000, agentNow, [][]string{{"e", request.ID}, {"p", f.member}, {"status", "done"}}, ""), agentNow), "invalid:", "payment-required")
}

func TestJobPolicyAndMembership(t *testing.T) {
	f := newJobFixture(t)
	ctx := context.Background()
	other, _ := event.PublicKey(agentOtherSecret)
	request := signed(t, agentOtherSecret, 5001, agentNow, [][]string{{"i", "hello", "text"}}, "")
	// The relay writes openly, but job kinds are for members and agents.
	expectError(t, f.gate.Write(ctx, request, relay.Session{PubKeys: []string{other}}, agentNow), "restricted:", "members and agents")
	if err := f.gate.Write(ctx, signed(t, agentOtherSecret, 1, agentNow, nil, "note"), relay.Session{PubKeys: []string{other}}, agentNow); err != nil {
		t.Fatalf("open relay refused a note: %v", err)
	}
	f.request(t, 5001, agentNow)
	if err := f.gate.Write(ctx, signed(t, agentOwnerSecret, 5002, agentNow, nil, ""), relay.Session{PubKeys: []string{f.owner}}, agentNow); err != nil {
		t.Fatalf("owner request rejected: %v", err)
	}
	// Switching long tasks off refuses every job kind, for members and on
	// import alike.
	p := f.gate.cfg.Policy()
	p.Features.Jobs = false
	f.gate.cfg.Policy = func() policy.Policy { return p }
	expectError(t, f.gate.Write(ctx, signed(t, jobMemberSecret, 5001, agentNow+1, nil, ""), relay.Session{PubKeys: []string{f.member}}, agentNow), "restricted:", "switched off")
	expectError(t, f.gate.Import(ctx, signed(t, jobMemberSecret, 7000, agentNow+1, [][]string{{"e", strings.Repeat("b", 64)}, {"p", f.member}, {"status", "success"}}, ""), agentNow), "restricted:", "switched off")
	if err := f.gate.Write(ctx, signed(t, jobMemberSecret, 1, agentNow+1, nil, "still fine"), relay.Session{PubKeys: []string{f.member}}, agentNow); err != nil {
		t.Fatalf("switching jobs off broke notes: %v", err)
	}
}

func TestJobGrantsRequestServeAndBoth(t *testing.T) {
	f := newJobFixture(t)
	ctx := context.Background()
	session := relay.Session{PubKeys: []string{f.agent}}
	request := f.request(t, 5001, agentNow)
	answer := func(kind int, extra ...[]string) event.Event {
		tags := append([][]string{{"e", request.ID}, {"p", f.member}}, extra...)
		return signed(t, agentSecret, kind, agentNow, tags, "output")
	}

	// request: 5xxx only.
	f.grant(t, agentNow, grantTags(f.agent, agentNow+3600, []string{"jobs", "request"}))
	if err := f.gate.Write(ctx, signed(t, agentSecret, 5001, agentNow, [][]string{{"i", "hello", "text"}}, ""), session, agentNow); err != nil {
		t.Fatalf("request grant refused a request: %v", err)
	}
	expectRestricted(t, f.gate.Write(ctx, answer(6001), session, agentNow), "kind 6001")
	expectRestricted(t, f.gate.Write(ctx, answer(7000, []string{"status", "processing"}), session, agentNow), "kind 7000")

	// serve: 6xxx and 7000 in reply to a held request; no requests.
	f.grant(t, agentNow+1, grantTags(f.agent, agentNow+3600, []string{"jobs", "serve"}))
	expectRestricted(t, f.gate.Write(ctx, signed(t, agentSecret, 5001, agentNow, nil, ""), session, agentNow), "kind 5001")
	if err := f.gate.Write(ctx, answer(6001, []string{"amount", "500"}), session, agentNow); err != nil {
		t.Fatalf("serve grant refused a result: %v", err)
	}
	if err := f.gate.Write(ctx, answer(7000, []string{"status", "payment-required"}, []string{"amount", "500", "lnbc1"}), session, agentNow); err != nil {
		t.Fatalf("serve grant refused feedback: %v", err)
	}
	unknown := signed(t, agentSecret, 6001, agentNow, [][]string{{"e", strings.Repeat("c", 64)}, {"p", f.member}}, "")
	expectRestricted(t, f.gate.Write(ctx, unknown, session, agentNow), "requests this relay holds")
	expectRestricted(t, f.gate.Import(ctx, unknown, agentNow), "requests this relay holds")
	// A note that is not a request cannot be answered, and an answer must
	// match the request's kind and author.
	note := signed(t, jobMemberSecret, 1, agentNow, nil, "not a job")
	if err := f.store.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := storage.SaveTx(ctx, tx, note, storage.SaveOptions{Now: agentNow})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	expectRestricted(t, f.gate.Write(ctx, signed(t, agentSecret, 6001, agentNow, [][]string{{"e", note.ID}, {"p", f.member}}, ""), session, agentNow), "requests this relay holds")
	expectError(t, f.gate.Write(ctx, answer(6002), session, agentNow), "invalid:", "does not answer")
	expectError(t, f.gate.Write(ctx, signed(t, agentSecret, 6001, agentNow, [][]string{{"e", request.ID}, {"p", f.owner}}, ""), session, agentNow), "invalid:", "name the requester")

	// both: requests and answers.
	f.grant(t, agentNow+2, grantTags(f.agent, agentNow+3600, []string{"jobs", "both"}))
	if err := f.gate.Write(ctx, signed(t, agentSecret, 5001, agentNow, nil, ""), session, agentNow); err != nil {
		t.Fatalf("both grant refused a request: %v", err)
	}
	if err := f.gate.Write(ctx, answer(6001), session, agentNow); err != nil {
		t.Fatalf("both grant refused a result: %v", err)
	}
	// A k tag still covers a single job kind, and answers from a k grant
	// name a held request as well.
	f.grant(t, agentNow+3, grantTags(f.agent, agentNow+3600, []string{"k", "5001"}, []string{"k", "6001"}))
	if err := f.gate.Write(ctx, signed(t, agentSecret, 5001, agentNow, nil, ""), session, agentNow); err != nil {
		t.Fatalf("k grant refused a request: %v", err)
	}
	expectRestricted(t, f.gate.Write(ctx, signed(t, agentSecret, 5002, agentNow, nil, ""), session, agentNow), "kind 5002")
	if err := f.gate.Write(ctx, answer(6001), session, agentNow); err != nil {
		t.Fatalf("k grant refused a result: %v", err)
	}
	expectRestricted(t, f.gate.Write(ctx, unknown, session, agentNow), "requests this relay holds")
	// The jobs tag is bounded.
	if _, err := community.ParseAgentGrant(signed(t, agentOwnerSecret, event.KIND_AGENT_GRANT, agentNow, grantTags(f.agent, agentNow+3600, []string{"jobs", "all"}), ""), agentNow); err == nil || !strings.Contains(err.Error(), "request, serve or both") {
		t.Fatalf("jobs tag accepted an unknown value: %v", err)
	}
	grant, err := community.ParseAgentGrant(signed(t, agentOwnerSecret, event.KIND_AGENT_GRANT, agentNow, grantTags(f.agent, agentNow+3600, []string{"jobs", "serve"}), ""), agentNow)
	if err != nil || grant.Scope.Jobs != community.AgentJobsServe || !grant.ServesJobs() || grant.RequestsJobs() {
		t.Fatalf("serve grant parsed as %+v: %v", grant.Scope, err)
	}
}

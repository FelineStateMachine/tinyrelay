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

// jobRoom is the fixture's slug room, which every member may write to.
const jobRoom = "general"

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

// request publishes a job request from the member asking the given keys
// and stores it, so answers can name a request the relay holds.
func (f jobFixture) request(t *testing.T, createdAt int64, asked ...string) event.Event {
	t.Helper()
	ctx := context.Background()
	tags := [][]string{{"h", jobRoom}, {"subject", "Summarize the notes"}}
	for _, key := range asked {
		tags = append(tags, []string{"p", key})
	}
	e := signed(t, jobMemberSecret, event.KIND_JOB_REQUEST, createdAt, tags, "Summarize this week's notes.")
	if err := f.gate.Write(ctx, e, relay.Session{PubKeys: []string{f.member}}, agentNow); err != nil {
		t.Fatalf("member request rejected: %v", err)
	}
	f.save(t, e)
	return e
}

func (f jobFixture) save(t *testing.T, e event.Event) {
	t.Helper()
	ctx := context.Background()
	if err := f.store.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := storage.SaveTx(ctx, tx, e, storage.SaveOptions{Now: agentNow})
		return err
	}); err != nil {
		t.Fatal(err)
	}
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
	request := f.request(t, agentNow, f.member)
	cases := []struct {
		name     string
		kind     int
		tags     [][]string
		fragment string
	}{
		{"request without a room", event.KIND_JOB_REQUEST, [][]string{{"p", f.member}}, "h tag"},
		{"request with a bad room", event.KIND_JOB_REQUEST, [][]string{{"h", "Not A Room"}, {"p", f.member}}, "h tag"},
		{"request without an asked key", event.KIND_JOB_REQUEST, [][]string{{"h", jobRoom}}, "at least one asked key"},
		{"request with a bad asked key", event.KIND_JOB_REQUEST, [][]string{{"h", jobRoom}, {"p", "abc"}}, "hex public keys"},
		{"request with a bad root", event.KIND_JOB_REQUEST, [][]string{{"h", jobRoom}, {"p", f.member}, {"e", "abc", "", "root"}}, "thread root"},
		{"accepted without a request", event.KIND_JOB_ACCEPTED, [][]string{{"h", jobRoom}, {"p", f.member}}, "e tag"},
		{"progress without a requester", event.KIND_JOB_PROGRESS, [][]string{{"h", jobRoom}, {"e", request.ID}}, "p tag"},
		{"result without a room", event.KIND_JOB_RESULT, [][]string{{"e", request.ID}, {"p", f.member}}, "h tag"},
		{"error with a bad room", event.KIND_JOB_ERROR, [][]string{{"h", "Not A Room"}, {"e", request.ID}, {"p", f.member}}, "h tag"},
		{"cancel without a request", event.KIND_JOB_CANCEL, [][]string{{"h", jobRoom}}, "e tag"},
		{"cancel without a room", event.KIND_JOB_CANCEL, [][]string{{"e", request.ID}}, "h tag"},
		{"cancel with a bad assignee", event.KIND_JOB_CANCEL, [][]string{{"h", jobRoom}, {"e", request.ID}, {"p", "abc"}}, "p tag"},
	}
	for _, tc := range cases {
		err := f.gate.Write(ctx, signed(t, jobMemberSecret, tc.kind, agentNow, tc.tags, ""), session, agentNow)
		expectError(t, err, "invalid:", tc.fragment)
	}
	// Well-formed answers from an asked member pass, with artifact
	// references on a result and a cancel from the requester.
	for _, e := range []event.Event{
		signed(t, jobMemberSecret, event.KIND_JOB_ACCEPTED, agentNow, [][]string{{"h", jobRoom}, {"e", request.ID}, {"p", f.member}}, ""),
		signed(t, jobMemberSecret, event.KIND_JOB_PROGRESS, agentNow, [][]string{{"h", jobRoom}, {"e", request.ID}, {"p", f.member}}, "halfway"),
		signed(t, jobMemberSecret, event.KIND_JOB_RESULT, agentNow, [][]string{{"h", jobRoom}, {"e", request.ID}, {"p", f.member}, {"e", strings.Repeat("a", 64)}, {"a", "30818:" + f.member + ":notes"}, {"r", "https://example.com/report"}}, "output"),
		signed(t, jobMemberSecret, event.KIND_JOB_ERROR, agentNow, [][]string{{"h", jobRoom}, {"e", request.ID}, {"p", f.member}}, "no such input"),
		signed(t, jobMemberSecret, event.KIND_JOB_CANCEL, agentNow, [][]string{{"h", jobRoom}, {"e", request.ID}, {"p", f.member}}, ""),
	} {
		if err := f.gate.Write(ctx, e, session, agentNow); err != nil {
			t.Fatalf("well-formed kind %d rejected: %v", e.Kind, err)
		}
	}
	// Import applies the same shape rules.
	expectError(t, f.gate.Import(ctx, signed(t, jobMemberSecret, event.KIND_JOB_PROGRESS, agentNow, [][]string{{"h", jobRoom}, {"e", request.ID}}, ""), agentNow), "invalid:", "p tag")
}

func TestJobAnswersFollowTheHeldRequest(t *testing.T) {
	f := newJobFixture(t)
	ctx := context.Background()
	session := relay.Session{PubKeys: []string{f.member}}
	other, _ := event.PublicKey(agentOtherSecret)
	// The owner is a member who was not asked; the member was.
	request := f.request(t, agentNow, f.member)
	answer := func(secret string, kind int, tags ...[]string) event.Event {
		return signed(t, secret, kind, agentNow, append([][]string{{"h", jobRoom}, {"e", request.ID}, {"p", f.member}}, tags...), "output")
	}
	// No answers to requests the relay does not hold, from members either.
	elsewhere := signed(t, jobMemberSecret, event.KIND_JOB_RESULT, agentNow, [][]string{{"h", jobRoom}, {"e", strings.Repeat("a", 64)}, {"p", other}}, "elsewhere")
	expectError(t, f.gate.Write(ctx, elsewhere, session, agentNow), "restricted:", "request this relay holds")
	expectError(t, f.gate.Import(ctx, elsewhere, agentNow), "restricted:", "request this relay holds")
	// A note that is not a request cannot be answered.
	note := signed(t, jobMemberSecret, 1, agentNow, nil, "not a job")
	f.save(t, note)
	expectError(t, f.gate.Write(ctx, signed(t, jobMemberSecret, event.KIND_JOB_RESULT, agentNow, [][]string{{"h", jobRoom}, {"e", note.ID}, {"p", f.member}}, "x"), session, agentNow), "restricted:", "request this relay holds")
	// The p tag names the requester and the h tag keeps the room.
	expectError(t, f.gate.Write(ctx, signed(t, jobMemberSecret, event.KIND_JOB_RESULT, agentNow, [][]string{{"h", jobRoom}, {"e", request.ID}, {"p", f.owner}}, "x"), session, agentNow), "invalid:", "name the requester")
	expectError(t, f.gate.Write(ctx, signed(t, jobMemberSecret, event.KIND_JOB_PROGRESS, agentNow, [][]string{{"h", "other-room"}, {"e", request.ID}, {"p", f.member}}, "x"), session, agentNow), "invalid:", "room")
	// Only a key the request asked may answer it.
	expectError(t, f.gate.Write(ctx, answer(agentOwnerSecret, event.KIND_JOB_PROGRESS), relay.Session{PubKeys: []string{f.owner}}, agentNow), "restricted:", "key the request asked")
	if err := f.gate.Write(ctx, answer(jobMemberSecret, event.KIND_JOB_PROGRESS), session, agentNow); err != nil {
		t.Fatalf("asked member's progress rejected: %v", err)
	}
	// Only the requester may cancel.
	cancel := func(secret string) event.Event {
		return signed(t, secret, event.KIND_JOB_CANCEL, agentNow, [][]string{{"h", jobRoom}, {"e", request.ID}}, "")
	}
	expectError(t, f.gate.Write(ctx, cancel(agentOwnerSecret), relay.Session{PubKeys: []string{f.owner}}, agentNow), "restricted:", "only the requester")
	if err := f.gate.Write(ctx, cancel(jobMemberSecret), session, agentNow); err != nil {
		t.Fatalf("requester's cancel rejected: %v", err)
	}
	expectError(t, f.gate.Write(ctx, signed(t, jobMemberSecret, event.KIND_JOB_CANCEL, agentNow, [][]string{{"h", "other-room"}, {"e", request.ID}}, ""), session, agentNow), "invalid:", "room")
}

func TestJobPolicyAndMembership(t *testing.T) {
	f := newJobFixture(t)
	ctx := context.Background()
	other, _ := event.PublicKey(agentOtherSecret)
	request := signed(t, agentOtherSecret, event.KIND_JOB_REQUEST, agentNow, [][]string{{"h", jobRoom}, {"p", f.member}}, "hello")
	// The relay writes openly, but job kinds are for members and agents.
	expectError(t, f.gate.Write(ctx, request, relay.Session{PubKeys: []string{other}}, agentNow), "restricted:", "members and agents")
	if err := f.gate.Write(ctx, signed(t, agentOtherSecret, 1, agentNow, nil, "note"), relay.Session{PubKeys: []string{other}}, agentNow); err != nil {
		t.Fatalf("open relay refused a note: %v", err)
	}
	f.request(t, agentNow, f.member)
	if err := f.gate.Write(ctx, signed(t, agentOwnerSecret, event.KIND_JOB_REQUEST, agentNow, [][]string{{"h", jobRoom}, {"p", f.member}}, "owner asks"), relay.Session{PubKeys: []string{f.owner}}, agentNow); err != nil {
		t.Fatalf("owner request rejected: %v", err)
	}
	// Switching long tasks off refuses every job kind, for members and on
	// import alike.
	p := f.gate.cfg.Policy()
	p.Features.Jobs = false
	f.gate.cfg.Policy = func() policy.Policy { return p }
	expectError(t, f.gate.Write(ctx, signed(t, jobMemberSecret, event.KIND_JOB_REQUEST, agentNow+1, [][]string{{"h", jobRoom}, {"p", f.member}}, "x"), relay.Session{PubKeys: []string{f.member}}, agentNow), "restricted:", "switched off")
	expectError(t, f.gate.Import(ctx, signed(t, jobMemberSecret, event.KIND_JOB_CANCEL, agentNow+1, [][]string{{"h", jobRoom}, {"e", strings.Repeat("b", 64)}}, ""), agentNow), "restricted:", "switched off")
	if err := f.gate.Write(ctx, signed(t, jobMemberSecret, 1, agentNow+1, nil, "still fine"), relay.Session{PubKeys: []string{f.member}}, agentNow); err != nil {
		t.Fatalf("switching jobs off broke notes: %v", err)
	}
}

func TestJobGrantsRequestServeAndBoth(t *testing.T) {
	f := newJobFixture(t)
	ctx := context.Background()
	session := relay.Session{PubKeys: []string{f.agent}}
	request := f.request(t, agentNow, f.agent)
	answer := func(kind int, extra ...[]string) event.Event {
		tags := append([][]string{{"h", jobRoom}, {"e", request.ID}, {"p", f.member}}, extra...)
		return signed(t, agentSecret, kind, agentNow, tags, "output")
	}
	ask := func(asked string) event.Event {
		return signed(t, agentSecret, event.KIND_JOB_REQUEST, agentNow, [][]string{{"h", jobRoom}, {"p", asked}}, "please")
	}

	// request: 43001 and 43005 only.
	f.grant(t, agentNow, grantTags(f.agent, agentNow+3600, []string{"room", jobRoom}, []string{"jobs", "request"}))
	if err := f.gate.Write(ctx, ask(f.member), session, agentNow); err != nil {
		t.Fatalf("request grant refused a request: %v", err)
	}
	own := ask(f.member)
	f.save(t, own)
	if err := f.gate.Write(ctx, signed(t, agentSecret, event.KIND_JOB_CANCEL, agentNow, [][]string{{"h", jobRoom}, {"e", own.ID}}, ""), session, agentNow); err != nil {
		t.Fatalf("request grant refused a cancel: %v", err)
	}
	expectRestricted(t, f.gate.Write(ctx, answer(event.KIND_JOB_RESULT), session, agentNow), "kind 43004")
	expectRestricted(t, f.gate.Write(ctx, answer(event.KIND_JOB_PROGRESS), session, agentNow), "kind 43003")

	// serve: answers to a held request that asked the agent; no requests
	// or cancels.
	f.grant(t, agentNow+1, grantTags(f.agent, agentNow+3600, []string{"room", jobRoom}, []string{"jobs", "serve"}))
	expectRestricted(t, f.gate.Write(ctx, ask(f.member), session, agentNow), "kind 43001")
	expectRestricted(t, f.gate.Write(ctx, signed(t, agentSecret, event.KIND_JOB_CANCEL, agentNow, [][]string{{"h", jobRoom}, {"e", own.ID}}, ""), session, agentNow), "kind 43005")
	for _, kind := range []int{event.KIND_JOB_ACCEPTED, event.KIND_JOB_PROGRESS, event.KIND_JOB_RESULT, event.KIND_JOB_ERROR} {
		if err := f.gate.Write(ctx, answer(kind), session, agentNow); err != nil {
			t.Fatalf("serve grant refused kind %d: %v", kind, err)
		}
	}
	unknown := signed(t, agentSecret, event.KIND_JOB_RESULT, agentNow, [][]string{{"h", jobRoom}, {"e", strings.Repeat("c", 64)}, {"p", f.member}}, "x")
	expectError(t, f.gate.Write(ctx, unknown, session, agentNow), "restricted:", "request this relay holds")
	expectError(t, f.gate.Import(ctx, unknown, agentNow), "restricted:", "request this relay holds")
	// A request that asked someone else cannot be answered by the agent.
	notAsked := f.request(t, agentNow+1, f.owner)
	expectError(t, f.gate.Write(ctx, signed(t, agentSecret, event.KIND_JOB_RESULT, agentNow, [][]string{{"h", jobRoom}, {"e", notAsked.ID}, {"p", f.member}}, "x"), session, agentNow), "restricted:", "key the request asked")
	expectError(t, f.gate.Write(ctx, signed(t, agentSecret, event.KIND_JOB_RESULT, agentNow, [][]string{{"h", jobRoom}, {"e", request.ID}, {"p", f.owner}}, "x"), session, agentNow), "invalid:", "name the requester")

	// both: requests and answers.
	f.grant(t, agentNow+2, grantTags(f.agent, agentNow+3600, []string{"room", jobRoom}, []string{"jobs", "both"}))
	if err := f.gate.Write(ctx, ask(f.member), session, agentNow); err != nil {
		t.Fatalf("both grant refused a request: %v", err)
	}
	if err := f.gate.Write(ctx, answer(event.KIND_JOB_RESULT), session, agentNow); err != nil {
		t.Fatalf("both grant refused a result: %v", err)
	}
	// A k tag still covers a single job kind, and answers from a k grant
	// name a held request as well.
	f.grant(t, agentNow+3, grantTags(f.agent, agentNow+3600, []string{"room", jobRoom}, []string{"k", "43001"}, []string{"k", "43004"}))
	if err := f.gate.Write(ctx, ask(f.member), session, agentNow); err != nil {
		t.Fatalf("k grant refused a request: %v", err)
	}
	expectRestricted(t, f.gate.Write(ctx, answer(event.KIND_JOB_PROGRESS), session, agentNow), "kind 43003")
	if err := f.gate.Write(ctx, answer(event.KIND_JOB_RESULT), session, agentNow); err != nil {
		t.Fatalf("k grant refused a result: %v", err)
	}
	expectError(t, f.gate.Write(ctx, unknown, session, agentNow), "restricted:", "request this relay holds")
	// The jobs tag is bounded.
	if _, err := community.ParseAgentGrant(signed(t, agentOwnerSecret, event.KIND_AGENT_GRANT, agentNow, grantTags(f.agent, agentNow+3600, []string{"jobs", "all"}), ""), agentNow); err == nil || !strings.Contains(err.Error(), "request, serve or both") {
		t.Fatalf("jobs tag accepted an unknown value: %v", err)
	}
	grant, err := community.ParseAgentGrant(signed(t, agentOwnerSecret, event.KIND_AGENT_GRANT, agentNow, grantTags(f.agent, agentNow+3600, []string{"jobs", "serve"}), ""), agentNow)
	if err != nil || grant.Scope.Jobs != community.AgentJobsServe || !grant.ServesJobs() || grant.RequestsJobs() {
		t.Fatalf("serve grant parsed as %+v: %v", grant.Scope, err)
	}
}

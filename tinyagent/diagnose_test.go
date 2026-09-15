package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
	"github.com/FelineStateMachine/tinyrelay/tinyagent/client"
)

// diagnoseRelay serves the three routes diagnose probes: readiness, the
// NIP-11 document and the signed browsegrant call. Each answer is scripted.
type diagnoseRelay struct {
	t         *testing.T
	pubkey    string
	prefix    string
	readyz    int
	nip11     int
	rpcStatus int
	rpcBody   string
	server    *httptest.Server
	mu        sync.Mutex
	requests  []string
	params    string
}

func newDiagnoseRelay(t *testing.T, prefix, pubkey string) *diagnoseRelay {
	t.Helper()
	d := &diagnoseRelay{t: t, pubkey: pubkey, prefix: prefix, readyz: http.StatusOK, nip11: http.StatusOK, rpcStatus: http.StatusOK, rpcBody: `{"result":{"member":true,"role":"agent","state":"active","enforced":true,"grant":{"pubkey":"` + pubkey + `","name":"helper"},"allows":{"kinds":[9],"rooms":["build"],"repos":[],"wiki":"","jobs":"","sites":[]},"rate":60,"expires":4102444800,"missing":{"rooms":[],"kinds":[]}}}`}
	d.server = httptest.NewServer(d)
	t.Cleanup(d.server.Close)
	return d
}

func (d *diagnoseRelay) base() string { return d.server.URL + d.prefix }

func (d *diagnoseRelay) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.requests...)
}

func (d *diagnoseRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.requests = append(d.requests, r.Method+" "+r.URL.Path)
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == d.prefix+"/readyz":
		if d.readyz == 0 {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(d.readyz)
		_, _ = io.WriteString(w, `{"status":"ready"}`)
	case r.Method == http.MethodGet && r.URL.Path == d.prefix+"/" && strings.Contains(r.Header.Get("Accept"), "application/nostr+json"):
		w.WriteHeader(d.nip11)
		_, _ = io.WriteString(w, `{"name":"test"}`)
	case r.Method == http.MethodPost && r.URL.Path == d.prefix+"/manage/rpc":
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(r.Header.Get("Content-Type"), "application/nostr+json+rpc") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid: content type"}`)
			return
		}
		if err := d.checkProof(r, body); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"auth-required: `+err.Error()+`"}`)
			return
		}
		var call struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &call); err != nil || call.Method != "browsegrant" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid: expected browsegrant"}`)
			return
		}
		d.mu.Lock()
		if len(call.Params) > 0 {
			d.params = string(call.Params[0])
		}
		d.mu.Unlock()
		w.WriteHeader(d.rpcStatus)
		_, _ = io.WriteString(w, d.rpcBody)
	default:
		http.NotFound(w, r)
	}
}

func (d *diagnoseRelay) checkProof(r *http.Request, body []byte) error {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Nostr ") {
		return errors.New("missing Nostr authorization")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Nostr "))
	if err != nil {
		return err
	}
	e, err := nostr.Parse(raw)
	if err != nil {
		return err
	}
	if err := nostr.Validate(e); err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	switch {
	case e.Kind != 27235:
		return errors.New("wrong kind")
	case e.PubKey != d.pubkey:
		return errors.New("wrong signer")
	case nostr.Tag(e, "u") != d.server.URL+r.URL.RequestURI():
		return errors.New("u tag " + nostr.Tag(e, "u"))
	case nostr.Tag(e, "method") != http.MethodPost:
		return errors.New("method tag")
	case nostr.Tag(e, "payload") != hex.EncodeToString(sum[:]):
		return errors.New("payload tag")
	}
	return nil
}

func runDiagnose(t *testing.T, relay string, extra ...string) (diagnosis, error) {
	t.Helper()
	var out bytes.Buffer
	args := append([]string{"diagnose", "--relay", relay, "--key-env", "TINY_MCP_TEST_KEY"}, extra...)
	err := run(args, strings.NewReader(""), &out, io.Discard)
	var report diagnosis
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &report); decodeErr != nil {
			t.Fatalf("diagnose output %s: %v", out.String(), decodeErr)
		}
	}
	return report, err
}

func TestDiagnoseReportsMissingGrantEntries(t *testing.T) {
	_, pub := testKey(t)
	relay := newDiagnoseRelay(t, "/r/acme", pub)
	relay.rpcBody = `{"result":{"member":true,"role":"agent","state":"active","enforced":true,"grant":{"pubkey":"` + pub + `","name":"helper"},"allows":{"kinds":[9],"rooms":["build"],"repos":[],"wiki":"","jobs":"","sites":[]},"rate":60,"expires":4102444800,"missing":{"rooms":["ops"],"kinds":[12]}}}`
	report, err := runDiagnose(t, relay.base(), "--rooms", "build, ops", "--kinds", "9,12")
	if exitStatus(err) != 2 || err == nil || !strings.HasPrefix(err.Error(), "needs-grant: call request_grant") {
		t.Fatalf("exit %d err %v", exitStatus(err), err)
	}
	if report.Verdict != verdictNeedsGrant || report.Advice != "call request_grant with rooms ops and kinds 12" {
		t.Fatalf("verdict %s advice %q", report.Verdict, report.Advice)
	}
	if report.PubKey != pub || report.Relay != relay.base() || !report.Transport.OK || report.Transport.Check != "readyz" || !report.Authentication.OK || report.Authentication.Status != 200 {
		t.Fatalf("probes %+v", report)
	}
	if !report.Member || report.Role != "agent" || report.State != "active" || !report.Enforced || report.Rate != 60 || string(report.Grant) == "null" || len(report.Allows) == 0 {
		t.Fatalf("grant fields %+v", report)
	}
	if report.Needs == nil || strings.Join(report.Needs.Rooms, ",") != "build,ops" || report.Missing == nil || strings.Join(report.Missing.Rooms, ",") != "ops" || len(report.Missing.Kinds) != 1 || report.Missing.Kinds[0] != 12 {
		t.Fatalf("needs %+v missing %+v", report.Needs, report.Missing)
	}
	if relay.params != `{"kinds":[9,12],"rooms":["build","ops"]}` {
		t.Fatalf("relay received params %s", relay.params)
	}
	if got := strings.Join(relay.seen(), ","); got != "GET /r/acme/readyz,POST /r/acme/manage/rpc" {
		t.Fatalf("requests %s", got)
	}
}

func TestDiagnoseOKThroughRPCMethod(t *testing.T) {
	secret, pub := testKey(t)
	relay := newDiagnoseRelay(t, "", pub)
	c, err := client.New(relay.base(), secret)
	if err != nil {
		t.Fatal(err)
	}
	server := &rpcServer{client: c, subs: map[string]context.CancelFunc{}, sem: make(chan struct{}, 1)}
	result, err := server.dispatch(context.Background(), "diagnose", json.RawMessage(`{"rooms":["build"],"kinds":[9]}`), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	report := result.(diagnosis)
	if report.Verdict != verdictOK || report.Advice != "the grant covers what was asked" || report.Missing == nil || len(report.Missing.Rooms) != 0 {
		t.Fatalf("report %+v", report)
	}
	if _, err := server.dispatch(context.Background(), "diagnose", json.RawMessage(`{"kinds":[-3]}`), io.Discard); err == nil {
		t.Fatal("bad kinds accepted")
	}
	// A human member is never held to a grant.
	relay.rpcBody = `{"result":{"member":true,"role":"member","state":"none","enforced":false,"grant":null}}`
	if report, err := runDiagnose(t, relay.base(), "--kinds", "9"); err != nil || report.Verdict != verdictOK || !strings.Contains(report.Advice, "member role") {
		t.Fatalf("member report %+v err %v", report, err)
	}
	// A member without a grant, and paused, revoked and expired grants, need
	// the operator.
	for _, state := range []string{"none", "paused", "revoked", "expired"} {
		relay.rpcBody = `{"result":{"member":true,"role":"agent","state":"` + state + `","enforced":true,"grant":null,"expires":1700000000}}`
		report, err := runDiagnose(t, relay.base())
		if exitStatus(err) != 2 || report.Verdict != verdictNeedsGrant || !strings.Contains(report.Advice, state) && !strings.Contains(report.Advice, "no agent grant") {
			t.Fatalf("%s: report %+v err %v", state, report, err)
		}
	}
}

func TestDiagnoseKeepsAuthorizationApartFromTransport(t *testing.T) {
	_, pub := testKey(t)
	relay := newDiagnoseRelay(t, "", pub)
	relay.rpcStatus, relay.rpcBody = http.StatusUnauthorized, `{"error":"auth-required: sign this request"}`
	report, err := runDiagnose(t, relay.base(), "--rooms", "build")
	if exitStatus(err) != 2 || report.Verdict != verdictUnauthorized || !report.Transport.OK || report.Authentication.OK || report.Authentication.Class != classAuthorization || report.Authentication.Status != 401 {
		t.Fatalf("401 report %+v err %v", report, err)
	}
	if !strings.Contains(report.Advice, "HTTP 401") || !strings.Contains(report.Authentication.Error, "auth-required") {
		t.Fatalf("401 advice %q error %q", report.Advice, report.Authentication.Error)
	}
	relay.rpcStatus, relay.rpcBody = http.StatusForbidden, `{"error":"restricted: this relay is members-only"}`
	report, err = runDiagnose(t, relay.base(), "--rooms", "build")
	if exitStatus(err) != 2 || report.Verdict != verdictNotMember || !report.Transport.OK || report.Authentication.Class != classAuthorization || !strings.Contains(report.Advice, "covering rooms build") {
		t.Fatalf("403 report %+v err %v", report, err)
	}
	relay.rpcStatus, relay.rpcBody = http.StatusOK, `{"result":{"member":false,"role":"","state":"none","enforced":true,"grant":null,"missing":{"rooms":["build"],"kinds":[]}}}`
	if report, err := runDiagnose(t, relay.base(), "--rooms", "build"); exitStatus(err) != 2 || report.Verdict != verdictNotMember || report.Member {
		t.Fatalf("stranger report %+v err %v", report, err)
	}
	// A private relay answers its NIP-11 document with 401: reachable.
	relay.readyz, relay.nip11 = 0, http.StatusUnauthorized
	relay.rpcBody = `{"result":{"member":true,"role":"agent","state":"active","enforced":true,"grant":{},"missing":{"rooms":[],"kinds":[]}}}`
	report, err = runDiagnose(t, relay.base(), "--rooms", "build")
	if err != nil || report.Verdict != verdictOK || !report.Transport.OK || report.Transport.Check != "nip11" || report.Transport.Status != 401 {
		t.Fatalf("private relay report %+v err %v", report, err)
	}
	// A relay that predates browsegrant still proves the signature works. It
	// answers HTTP 400 with the unsupported-method error its dispatcher
	// writes for a name it does not know.
	for _, body := range []string{`{"error":"unsupported: unknown community method \"browsegrant\""}`, `{"error":"unsupported: browse operation"}`} {
		relay.rpcStatus, relay.rpcBody = http.StatusBadRequest, body
		report, err = runDiagnose(t, relay.base())
		if err != nil || report.Verdict != verdictOK || !report.Authentication.OK || report.Authentication.Status != 400 || !strings.Contains(report.Advice, "does not answer browsegrant") {
			t.Fatalf("old relay report %+v err %v", report, err)
		}
	}
	// A 5xx on the signed call is transport, not authorization.
	relay.rpcStatus, relay.rpcBody = http.StatusBadGateway, `upstream down`
	report, err = runDiagnose(t, relay.base())
	if exitStatus(err) != 3 || report.Verdict != verdictUnreachable || report.Authentication.Class != classTransport || !report.Transport.OK {
		t.Fatalf("502 report %+v err %v", report, err)
	}
}

// TestDiagnoseNeverReadsOtherClientErrorsAsOK is the review's loopback
// probe: a 429 or a 400 that is not the unsupported-method error must not
// be reported as a working signature on an old relay.
func TestDiagnoseNeverReadsOtherClientErrorsAsOK(t *testing.T) {
	_, pub := testKey(t)
	relay := newDiagnoseRelay(t, "", pub)
	cases := []struct {
		name    string
		status  int
		body    string
		class   string
		verdict string
		advice  string
	}{
		{"rate limited", http.StatusTooManyRequests, `{"error":"rate limited"}`, classRateLimited, verdictUnreachable, "rate-limited"},
		{"invalid params", http.StatusBadRequest, `{"error":"invalid: rooms must be room ids"}`, classProtocol, verdictError, "HTTP 400"},
		{"not found", http.StatusNotFound, `not here`, classProtocol, verdictError, "HTTP 404"},
		{"plain 400", http.StatusBadRequest, ``, classProtocol, verdictError, "HTTP 400"},
	}
	for _, tc := range cases {
		relay.rpcStatus, relay.rpcBody = tc.status, tc.body
		report, err := runDiagnose(t, relay.base(), "--rooms", "build")
		if exitStatus(err) != 3 || err == nil {
			t.Fatalf("%s: exit %d err %v", tc.name, exitStatus(err), err)
		}
		if report.Verdict != tc.verdict || !report.Transport.OK || report.Authentication.OK || report.Authentication.Class != tc.class || report.Authentication.Status != tc.status {
			t.Fatalf("%s: report %+v", tc.name, report)
		}
		if !strings.Contains(report.Advice, tc.advice) || strings.Contains(report.Advice, "does not answer browsegrant") {
			t.Fatalf("%s: advice %q", tc.name, report.Advice)
		}
	}
	if diagnoseExit(verdictError) != 3 {
		t.Fatalf("error verdict exits %d", diagnoseExit(verdictError))
	}
}

func TestDiagnoseUnreachableRelay(t *testing.T) {
	_, pub := testKey(t)
	relay := newDiagnoseRelay(t, "", pub)
	base := relay.base()
	relay.server.Close()
	report, err := runDiagnose(t, base, "--kinds", "9")
	if exitStatus(err) != 3 || err == nil || !strings.HasPrefix(err.Error(), "unreachable: ") {
		t.Fatalf("exit %d err %v", exitStatus(err), err)
	}
	if report.Verdict != verdictUnreachable || report.Transport.OK || report.Transport.Class != classTransport || report.Transport.Error == "" || report.Authentication.OK || report.Authentication.Class != "" {
		t.Fatalf("report %+v", report)
	}
	if report.PubKey != pub || report.Needs == nil || report.Needs.Kinds[0] != 9 {
		t.Fatalf("identity and needs %+v", report)
	}
	if _, err := runDiagnose(t, base, "--kinds", "nine"); err == nil || exitStatus(err) != 1 {
		t.Fatalf("bad kinds flag: %v", err)
	}
	if err := run([]string{"diagnose"}, strings.NewReader(""), io.Discard, io.Discard); err == nil || !strings.HasPrefix(err.Error(), "usage: tinyagent diagnose") {
		t.Fatalf("usage: %v", err)
	}
}

func TestMCPDiagnoseToolAnswersWithoutTheRelayTable(t *testing.T) {
	_, pub := testKey(t)
	relay := newDiagnoseRelay(t, "/r/acme", pub)
	relay.rpcBody = `{"result":{"member":true,"role":"agent","state":"active","enforced":true,"grant":{"name":"helper"},"allows":{"kinds":[9],"rooms":["build"],"repos":[],"wiki":"","jobs":"","sites":[]},"missing":{"rooms":[],"kinds":[20001]}}}`
	c := startFacade(t, "--relay", relay.base(), "--key-env", "TINY_MCP_TEST_KEY")
	c.initialize("2025-06-18")
	result, response := c.tool(mcpDiagnoseTool, map[string]any{"rooms": []any{"build"}, "kinds": []any{9, 20001}})
	if response.Error != nil || result.IsError {
		t.Fatalf("tiny_diagnose: %+v %+v", response.Error, result)
	}
	if firstText(result) != "needs-grant: call request_grant with kinds 20001" {
		t.Fatalf("summary %q", firstText(result))
	}
	structured, _ := result.StructuredContent.(map[string]any)
	if structured["verdict"] != verdictNeedsGrant || structured["pubkey"] != pub || structured["state"] != "active" {
		t.Fatalf("structured %v", structured)
	}
	if relay.params != `{"kinds":[9,20001],"rooms":["build"]}` {
		t.Fatalf("relay received params %s", relay.params)
	}
	if result, _ := c.tool(mcpDiagnoseTool, map[string]any{"kinds": "nine"}); !result.IsError || !strings.Contains(firstText(result), "kinds must be") {
		t.Fatalf("bad arguments: %+v", result)
	}
	for _, path := range relay.seen() {
		if strings.HasSuffix(path, "/mcp") {
			t.Fatalf("diagnosis touched the MCP endpoint: %v", relay.seen())
		}
	}
}

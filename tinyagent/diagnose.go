package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
	"github.com/FelineStateMachine/tinyrelay/tinyagent/client"
)

// Diagnostics answer one question for an agent and its operator: can this
// key do what the connector needs on this relay, and if not, is the relay
// out of reach, is the key refused, or is the grant too narrow. Transport
// failures and authorization failures are kept apart throughout: a signed
// request the relay answers, whatever the status, proves the relay is
// reachable.

const (
	verdictOK           = "ok"
	verdictNeedsGrant   = "needs-grant"
	verdictNotMember    = "not-a-member"
	verdictUnauthorized = "unauthorized"
	verdictUnreachable  = "unreachable"
	verdictError        = "error"

	classTransport     = "transport"
	classAuthorization = "authorization"
	classRateLimited   = "rate-limited"
	classProtocol      = "protocol"

	diagnoseProbeTimeout = 10 * time.Second
	diagnoseBodyLimit    = 1 << 20
)

// diagnoseNeeds names the rooms and kinds the connector wants to use.
type diagnoseNeeds struct {
	Rooms []string `json:"rooms"`
	Kinds []int    `json:"kinds"`
}

// diagnoseCheck is one probe's outcome. Class is set on failure only.
type diagnoseCheck struct {
	OK     bool   `json:"ok"`
	Check  string `json:"check,omitempty"`
	Status int    `json:"status,omitempty"`
	Class  string `json:"class,omitempty"`
	Error  string `json:"error,omitempty"`
}

// diagnoseMissing lists what the grant lacks among the needs.
type diagnoseMissing struct {
	Rooms []string `json:"rooms"`
	Kinds []int    `json:"kinds"`
}

// diagnosis is the report written by `tinyagent diagnose`, the diagnose
// RPC method and the tiny_diagnose MCP tool.
type diagnosis struct {
	Relay          string           `json:"relay"`
	PubKey         string           `json:"pubkey"`
	Transport      diagnoseCheck    `json:"transport"`
	Authentication diagnoseCheck    `json:"authentication"`
	Member         bool             `json:"member"`
	Role           string           `json:"role"`
	Grant          json.RawMessage  `json:"grant"`
	State          string           `json:"state"`
	Enforced       bool             `json:"enforced"`
	Allows         json.RawMessage  `json:"allows,omitempty"`
	Rate           int              `json:"rate,omitempty"`
	Expires        int64            `json:"expires,omitempty"`
	Needs          *diagnoseNeeds   `json:"needs,omitempty"`
	Missing        *diagnoseMissing `json:"missing,omitempty"`
	Verdict        string           `json:"verdict"`
	Advice         string           `json:"advice"`
}

// grantAnswer is the relay's browsegrant result.
type grantAnswer struct {
	Member   bool             `json:"member"`
	Role     string           `json:"role"`
	Grant    json.RawMessage  `json:"grant"`
	State    string           `json:"state"`
	Enforced bool             `json:"enforced"`
	Allows   json.RawMessage  `json:"allows"`
	Rate     int              `json:"rate"`
	Expires  int64            `json:"expires"`
	Missing  *diagnoseMissing `json:"missing"`
}

// exitError carries a process exit status through run.
type exitError struct {
	code    int
	message string
}

func (e *exitError) Error() string { return e.message }

// exitStatus is the process status for an error from run: 1 unless the
// error names its own code.
func exitStatus(err error) int {
	var exit *exitError
	if errors.As(err, &exit) {
		return exit.code
	}
	return 1
}

// diagnoseExit maps a verdict to the documented exit status.
func diagnoseExit(verdict string) int {
	switch verdict {
	case verdictOK:
		return 0
	case verdictUnreachable, verdictError:
		return 3
	default:
		return 2
	}
}

func diagnoseCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	relay := fs.String("relay", "", "relay URL")
	keyEnv := fs.String("key-env", "TINY_PRIVATE_KEY", "private key environment variable")
	rooms := fs.String("rooms", "", "comma-separated room ids the connector needs")
	kinds := fs.String("kinds", "", "comma-separated event kinds the connector needs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *relay == "" {
		return errors.New("usage: tinyagent diagnose --relay URL [--key-env TINY_PRIVATE_KEY] [--rooms a,b] [--kinds 9,12]")
	}
	secret := os.Getenv(*keyEnv)
	if secret == "" {
		return fmt.Errorf("%s is not set", *keyEnv)
	}
	c, err := client.New(*relay, secret)
	if err != nil {
		return err
	}
	needs, err := parseNeedsFlags(*rooms, *kinds)
	if err != nil {
		return err
	}
	report := diagnose(context.Background(), c, needs)
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return err
	}
	if code := diagnoseExit(report.Verdict); code != 0 {
		return &exitError{code: code, message: report.Verdict + ": " + report.Advice}
	}
	return nil
}

func parseNeedsFlags(rooms, kinds string) (diagnoseNeeds, error) {
	var needs diagnoseNeeds
	for _, room := range strings.Split(rooms, ",") {
		if room = strings.TrimSpace(room); room != "" {
			needs.Rooms = append(needs.Rooms, room)
		}
	}
	for _, raw := range strings.Split(kinds, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		kind, err := strconv.Atoi(raw)
		if err != nil || kind < 0 || kind > 65535 {
			return needs, fmt.Errorf("--kinds: %q is not an event kind", raw)
		}
		needs.Kinds = append(needs.Kinds, kind)
	}
	return needs, nil
}

// parseNeedsParams reads rooms and kinds from RPC params or tool arguments.
func parseNeedsParams(raw map[string]json.RawMessage) (diagnoseNeeds, error) {
	var needs diagnoseNeeds
	if len(raw["rooms"]) > 0 {
		if err := json.Unmarshal(raw["rooms"], &needs.Rooms); err != nil {
			return needs, errors.New("rooms must be an array of room ids")
		}
	}
	if len(raw["kinds"]) > 0 {
		if err := json.Unmarshal(raw["kinds"], &needs.Kinds); err != nil {
			return needs, errors.New("kinds must be an array of event kinds")
		}
	}
	for _, room := range needs.Rooms {
		if strings.TrimSpace(room) == "" {
			return needs, errors.New("rooms must be room ids")
		}
	}
	for _, kind := range needs.Kinds {
		if kind < 0 || kind > 65535 {
			return needs, errors.New("kinds must be event kinds")
		}
	}
	return needs, nil
}

// diagnose runs the probes in order and stops at the first one that rules
// out the rest: an unreachable relay is not asked to authenticate, and a
// refused key is not asked about its grant.
func (s *rpcServer) diagnose(ctx context.Context, raw map[string]json.RawMessage) (any, error) {
	needs, err := parseNeedsParams(raw)
	if err != nil {
		return nil, err
	}
	return diagnose(ctx, s.client, needs), nil
}

func diagnose(ctx context.Context, c *client.Client, needs diagnoseNeeds) diagnosis {
	report := diagnosis{Relay: c.BaseURL, PubKey: c.PubKey, Grant: json.RawMessage("null"), State: "none"}
	if len(needs.Rooms) > 0 || len(needs.Kinds) > 0 {
		copied := diagnoseNeeds{Rooms: append([]string{}, needs.Rooms...), Kinds: append([]int{}, needs.Kinds...)}
		report.Needs = &copied
	}
	report.Transport = probeTransport(ctx, c)
	if !report.Transport.OK {
		report.Verdict = verdictUnreachable
		report.Advice = "the relay at " + c.BaseURL + " did not answer: check the URL, the network and that the relay is running (" + report.Transport.Error + ")"
		return report
	}
	answer, check := probeGrant(ctx, c, needs)
	report.Authentication = check
	if !check.OK {
		switch check.Class {
		case classAuthorization:
			if check.Status == http.StatusForbidden && strings.Contains(check.Error, "members-only") {
				report.Verdict = verdictNotMember
				report.Advice = "the relay is members-only and this key is not a member: ask the owner for membership or an agent grant" + needsClause(needs)
			} else {
				report.Verdict = verdictUnauthorized
				report.Advice = "the relay refused the signed request (HTTP " + strconv.Itoa(check.Status) + "): check that TINY_PRIVATE_KEY is the agent key the relay expects and that --relay is the exact origin the relay serves (" + check.Error + ")"
			}
		case classRateLimited:
			report.Verdict = verdictUnreachable
			report.Advice = "the relay rate-limited the signed request (HTTP 429): retry later (" + check.Error + ")"
		case classProtocol:
			report.Verdict = verdictError
			report.Advice = "the relay answered the signed browsegrant call with " + check.Error + " instead of a result: check that --relay is the exact origin the relay serves, with its tenant prefix, and that the relay is current"
		default:
			report.Verdict = verdictUnreachable
			report.Advice = "the relay answered its health check but not a signed request: " + check.Error
		}
		return report
	}
	if answer == nil {
		// The signature was accepted but the relay predates browsegrant.
		report.Verdict = verdictOK
		report.Advice = "the relay accepts this key's signature but does not answer browsegrant, so its grant cannot be read: upgrade the relay to see grant state"
		return report
	}
	report.Member, report.Role, report.State, report.Enforced = answer.Member, answer.Role, answer.State, answer.Enforced
	report.Rate, report.Expires = answer.Rate, answer.Expires
	if len(answer.Grant) > 0 {
		report.Grant = answer.Grant
	}
	if len(answer.Allows) > 0 && string(answer.Allows) != "null" {
		report.Allows = answer.Allows
	}
	if answer.Missing != nil {
		report.Missing = answer.Missing
	} else if report.Needs != nil {
		report.Missing = &diagnoseMissing{Rooms: []string{}, Kinds: []int{}}
	}
	report.Verdict, report.Advice = grantVerdict(answer, needs)
	return report
}

// grantVerdict reads a browsegrant answer against the needs.
func grantVerdict(answer *grantAnswer, needs diagnoseNeeds) (string, string) {
	if !answer.Member {
		return verdictNotMember, "this key is not a member of the relay: ask the owner for an agent grant" + needsClause(needs)
	}
	if !answer.Enforced {
		return verdictOK, "this key holds the " + answer.Role + " role; agent grants do not apply to it"
	}
	switch answer.State {
	case "paused":
		return verdictNeedsGrant, "the agent grant is paused: ask the operator to resume it"
	case "revoked":
		return verdictNeedsGrant, "the agent grant is revoked: ask the operator for a new grant" + needsClause(needs)
	case "expired":
		return verdictNeedsGrant, "the agent grant expired at " + time.Unix(answer.Expires, 0).UTC().Format(time.RFC3339) + ": ask the operator for a new grant" + needsClause(needs)
	case "none":
		return verdictNeedsGrant, "this key is a member but holds no agent grant: ask the operator for one" + needsClause(needs)
	}
	if answer.Missing != nil && (len(answer.Missing.Rooms) > 0 || len(answer.Missing.Kinds) > 0) {
		return verdictNeedsGrant, "call request_grant with " + missingClause(*answer.Missing)
	}
	return verdictOK, "the grant covers what was asked"
}

func needsClause(needs diagnoseNeeds) string {
	if len(needs.Rooms) == 0 && len(needs.Kinds) == 0 {
		return ""
	}
	return " covering " + missingClause(diagnoseMissing(needs))
}

func missingClause(missing diagnoseMissing) string {
	parts := []string{}
	if len(missing.Rooms) > 0 {
		parts = append(parts, "rooms "+strings.Join(missing.Rooms, ", "))
	}
	if len(missing.Kinds) > 0 {
		kinds := make([]string, len(missing.Kinds))
		for i, kind := range missing.Kinds {
			kinds[i] = strconv.Itoa(kind)
		}
		parts = append(parts, "kinds "+strings.Join(kinds, ", "))
	}
	return strings.Join(parts, " and ")
}

// probeTransport asks the relay whether it is up without a signature. The
// readiness route answers first; a relay that lacks it or is not ready is
// still reachable when its NIP-11 document answers with any status below
// 500, including 401 on a private relay.
func probeTransport(ctx context.Context, c *client.Client) diagnoseCheck {
	status, _, err := diagnoseGet(ctx, c, "/readyz", "application/json")
	if err == nil && status/100 == 2 {
		return diagnoseCheck{OK: true, Check: "readyz", Status: status}
	}
	readyz := describeProbe(status, err)
	status, _, err = diagnoseGet(ctx, c, "/", "application/nostr+json")
	if err == nil && status < 500 {
		return diagnoseCheck{OK: true, Check: "nip11", Status: status}
	}
	return diagnoseCheck{OK: false, Check: "nip11", Status: status, Class: classTransport, Error: "readyz: " + readyz + "; nip11: " + describeProbe(status, err)}
}

func describeProbe(status int, err error) string {
	if err != nil {
		return err.Error()
	}
	return "HTTP " + strconv.Itoa(status)
}

// probeGrant makes the signed browsegrant call. A nil answer with an ok
// check means the relay accepted the signature but has no such method: it
// answered HTTP 400 with the unsupported-method error its dispatcher writes
// for a name it does not know. 401 and 403 are authorization failures, 429
// is rate limiting, 5xx is transport, and any other status is a protocol
// error; none of those is read as a working signature.
func probeGrant(ctx context.Context, c *client.Client, needs diagnoseNeeds) (*grantAnswer, diagnoseCheck) {
	params := map[string]any{}
	if len(needs.Rooms) > 0 {
		params["rooms"] = needs.Rooms
	}
	if len(needs.Kinds) > 0 {
		params["kinds"] = needs.Kinds
	}
	body, err := json.Marshal(map[string]any{"method": "browsegrant", "params": []any{params}})
	if err != nil {
		return nil, diagnoseCheck{Class: classTransport, Error: err.Error()}
	}
	status, data, err := diagnoseSigned(ctx, c, "/manage/rpc", body)
	if err != nil {
		return nil, diagnoseCheck{Class: classTransport, Error: err.Error()}
	}
	var envelope struct {
		Result *grantAnswer `json:"result"`
		Error  string       `json:"error"`
	}
	_ = json.Unmarshal(data, &envelope)
	message := envelope.Error
	if message == "" {
		message = strings.TrimSpace(string(data))
	}
	if message == "" {
		message = http.StatusText(status)
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, diagnoseCheck{Status: status, Class: classAuthorization, Error: message}
	case status == http.StatusTooManyRequests:
		return nil, diagnoseCheck{Status: status, Class: classRateLimited, Error: message}
	case status >= 500:
		return nil, diagnoseCheck{Status: status, Class: classTransport, Error: "HTTP " + strconv.Itoa(status) + ": " + message}
	case status == http.StatusBadRequest && strings.HasPrefix(envelope.Error, "unsupported:"):
		// The signature was checked before the method was dispatched, and
		// the dispatcher did not know browsegrant.
		return nil, diagnoseCheck{OK: true, Status: status, Error: message}
	case status/100 != 2:
		return nil, diagnoseCheck{Status: status, Class: classProtocol, Error: "HTTP " + strconv.Itoa(status) + ": " + message}
	case envelope.Result == nil:
		return nil, diagnoseCheck{Status: status, Class: classTransport, Error: "the relay returned an invalid browsegrant result"}
	}
	return envelope.Result, diagnoseCheck{OK: true, Status: status}
}

func diagnoseGet(ctx context.Context, c *client.Client, p, accept string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, diagnoseProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+p, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", accept)
	return diagnoseDo(c, req)
}

// diagnoseSigned posts a NIP-86 body with a fresh NIP-98 proof bound to the
// method, the full URL and the body hash, the same binding the client uses.
func diagnoseSigned(ctx context.Context, c *client.Client, p string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, diagnoseProbeTimeout)
	defer cancel()
	target := c.BaseURL + p
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return 0, nil, fmt.Errorf("generate request nonce: %w", err)
	}
	sum := sha256.Sum256(body)
	proof := nostr.Event{Kind: 27235, CreatedAt: time.Now().Unix(), Tags: [][]string{{"u", target}, {"method", http.MethodPost}, {"nonce", hex.EncodeToString(nonce)}, {"payload", hex.EncodeToString(sum[:])}}}
	if err := nostr.Sign(&proof, c.Secret); err != nil {
		return 0, nil, err
	}
	canonical, err := nostr.Canonical(proof)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Nostr "+base64.StdEncoding.EncodeToString(canonical))
	req.Header.Set("Content-Type", "application/nostr+json+rpc")
	req.Header.Set("Accept", "application/json")
	return diagnoseDo(c, req)
}

func diagnoseDo(c *client.Client, req *http.Request) (int, []byte, error) {
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, diagnoseBodyLimit))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

// diagnoseSummary is the one-line text form of a report for tool output.
func diagnoseSummary(report diagnosis) string {
	return report.Verdict + ": " + report.Advice
}

// diagnose answers the tiny_diagnose tool: the report as structured content
// with its verdict and advice as the text block. The report is an answer,
// not a failure, whatever the verdict.
func (f *mcpFacade) diagnose(ctx context.Context, arguments map[string]any) mcpToolResult {
	raw := make(map[string]json.RawMessage, len(arguments))
	for key, value := range arguments {
		encoded, err := json.Marshal(value)
		if err != nil {
			return mcpFailure(key + " is not JSON")
		}
		raw[key] = encoded
	}
	needs, err := parseNeedsParams(raw)
	if err != nil {
		return mcpFailure(err.Error())
	}
	f.relayMu.Lock()
	defer f.relayMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
	defer cancel()
	report := diagnose(ctx, f.client, needs)
	return mcpToolResult{Content: []map[string]any{{"type": "text", "text": diagnoseSummary(report)}}, StructuredContent: report}
}

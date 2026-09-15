package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/FelineStateMachine/tinyrelay/internal/mcp"
	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
	"github.com/FelineStateMachine/tinyrelay/tinyagent/client"
)

// The facade speaks the standard MCP stdio protocol to Hermes and forwards
// every tool to the relay's stateless HTTP endpoint with fresh NIP-98
// signatures. Only the curated tools below are ever exposed; publish_event
// and the management tools never are.

// mcpReadTools are exposed by default.
var mcpReadTools = []string{
	"list_repositories", "read_repository",
	"list_issues", "read_issue",
	"list_pull_requests", "read_pull_request",
	"list_files", "read_file", "read_attachment",
	"list_rooms", "read_room", "read_thread",
	"list_wiki", "read_wiki_page",
	"list_jobs", "read_job",
	"read_status",
}

// mcpWriteTools are added with --allow-writes.
var mcpWriteTools = []string{
	"create_issue", "create_pull_request", "comment",
	"post_message", "start_thread", "reply_in_thread", "react",
	"publish_wiki_page", "propose_wiki_merge",
	"upload_attachment",
	"request_decision", "request_grant",
	"request_job", "accept_job", "job_progress", "job_result", "job_error", "cancel_job",
}

// mcpHandshakeVersions are the revisions the official SDK's stdio client may
// send in initialize. Any other request is answered with mcpFallbackVersion.
var mcpHandshakeVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"}

const (
	mcpFallbackVersion = "2025-06-18"
	mcpTextLimit       = 64 << 10
	mcpCallTimeout     = 2 * time.Minute
	mcpPermission      = "permission: "
	mcpTransport       = "transport: "
)

// mcpAuthKinds are never signed on the relay's behalf: a template of one of
// these kinds would be an authorization, not a publication.
var mcpAuthKinds = map[int]bool{22242: true, 24242: true, 27235: true}

type mcpFacade struct {
	client  *client.Client
	secret  string
	allowed map[string]bool
	order   []string
	info    mcp.Implementation
	stderr  io.Writer
	relayMu sync.Mutex
	tableMu sync.Mutex
	table   []map[string]any
	outMu   sync.Mutex
}

// mcpToolResult is the subset of a tools/call result the facade relays.
type mcpToolResult struct {
	Content           []map[string]any `json:"content"`
	StructuredContent any              `json:"structuredContent,omitempty"`
	IsError           bool             `json:"isError"`
}

func mcpCommand(args []string, in io.Reader, out, stderr io.Writer) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	relay := fs.String("relay", "", "relay URL")
	keyEnv := fs.String("key-env", "TINY_PRIVATE_KEY", "private key environment variable")
	tools := fs.String("tools", "", "comma-separated tool names to expose")
	writes := fs.Bool("allow-writes", false, "expose write tools")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *relay == "" {
		return errors.New("usage: tinyagent mcp --relay URL [--key-env TINY_PRIVATE_KEY] [--tools name,name] [--allow-writes]")
	}
	secret := os.Getenv(*keyEnv)
	if secret == "" {
		return fmt.Errorf("%s is not set", *keyEnv)
	}
	c, err := client.New(*relay, secret)
	if err != nil {
		return err
	}
	order, err := mcpAllowlist(*tools, *writes)
	if err != nil {
		return err
	}
	f := newMCPFacade(c, secret, order, stderr)
	fmt.Fprintf(stderr, "tinyagent mcp: relay %s, %d tools\n", c.BaseURL, len(order))
	return f.serve(in, out)
}

func newMCPFacade(c *client.Client, secret string, order []string, stderr io.Writer) *mcpFacade {
	allowed := make(map[string]bool, len(order))
	for _, name := range order {
		allowed[name] = true
	}
	return &mcpFacade{client: c, secret: secret, allowed: allowed, order: order, info: mcp.Implementation{Name: "tinyagent", Version: tinyagentVersion()}, stderr: stderr}
}

// mcpAllowlist resolves the exposed tool names. Without --tools the read
// tools are exposed, plus the write tools with --allow-writes. A --tools
// list may name any curated tool; write tools in it still need
// --allow-writes.
func mcpAllowlist(spec string, writes bool) ([]string, error) {
	reads := map[string]bool{}
	for _, name := range mcpReadTools {
		reads[name] = true
	}
	writeSet := map[string]bool{}
	for _, name := range mcpWriteTools {
		writeSet[name] = true
	}
	if strings.TrimSpace(spec) == "" {
		order := append([]string(nil), mcpReadTools...)
		if writes {
			order = append(order, mcpWriteTools...)
		}
		return order, nil
	}
	var order []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(spec, ",") {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] {
			continue
		}
		switch {
		case reads[name]:
		case writeSet[name]:
			if !writes {
				return nil, fmt.Errorf("tool %s publishes events; add --allow-writes to expose it", name)
			}
		default:
			return nil, fmt.Errorf("unknown tool %q in --tools", name)
		}
		seen[name] = true
		order = append(order, name)
	}
	if len(order) == 0 {
		return nil, errors.New("--tools names no tool")
	}
	return order, nil
}

func tinyagentVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

// serve reads newline-delimited JSON-RPC messages from in and answers on
// out. Requests run concurrently so pings are answered while a relay call
// is in flight; relay calls themselves are serialized. Notifications,
// including cancellations, are ignored.
func (f *mcpFacade) serve(in io.Reader, out io.Writer) error {
	scan := bufio.NewScanner(in)
	scan.Buffer(make([]byte, 4096), 48<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for scan.Scan() {
		line := bytes.TrimSpace(scan.Bytes())
		if len(line) == 0 {
			continue
		}
		var req mcp.Request
		if err := json.Unmarshal(line, &req); err != nil || !bytes.HasPrefix(line, []byte("{")) {
			f.write(out, mcp.Response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &mcp.Error{Code: mcp.CodeParse, Message: "Parse error: the message is not one JSON-RPC object"}})
			continue
		}
		if req.Notification() || string(bytes.TrimSpace(req.ID)) == "null" {
			continue
		}
		wg.Add(1)
		go func(req mcp.Request) {
			defer wg.Done()
			f.write(out, f.handle(ctx, req))
		}(req)
	}
	wg.Wait()
	return scan.Err()
}

func (f *mcpFacade) write(out io.Writer, response mcp.Response) {
	f.outMu.Lock()
	defer f.outMu.Unlock()
	raw, err := json.Marshal(response)
	if err != nil {
		fmt.Fprintf(f.stderr, "tinyagent mcp: encode response: %v\n", err)
		return
	}
	_, _ = out.Write(append(raw, '\n'))
}

func (f *mcpFacade) handle(ctx context.Context, req mcp.Request) mcp.Response {
	response := mcp.Response{JSONRPC: "2.0", ID: req.ID}
	if req.JSONRPC != "2.0" || req.Method == "" {
		response.Error = &mcp.Error{Code: mcp.CodeInvalidRequest, Message: "jsonrpc must be \"2.0\" and method is required"}
		return response
	}
	var result any
	var rpcErr *mcp.Error
	switch req.Method {
	case "initialize":
		result = f.initialize(req.Params)
	case "ping":
		result = struct{}{}
	case "tools/list":
		result, rpcErr = f.list(ctx)
	case "tools/call":
		result, rpcErr = f.call(ctx, req.Params)
	default:
		rpcErr = &mcp.Error{Code: mcp.CodeMethodNotFound, Message: "Method not found: " + req.Method}
	}
	if rpcErr != nil {
		response.Error = rpcErr
		return response
	}
	response.Result = result
	return response
}

func (f *mcpFacade) initialize(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	version := mcpFallbackVersion
	for _, known := range mcpHandshakeVersions {
		if p.ProtocolVersion == known {
			version = known
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      f.info,
	}
}

func (f *mcpFacade) list(ctx context.Context) (map[string]any, *mcp.Error) {
	tools, err := f.tools(ctx)
	if err != nil {
		return nil, &mcp.Error{Code: mcp.CodeInternal, Message: f.describe(err)}
	}
	return map[string]any{"tools": tools}, nil
}

// tools returns the relay's tool table restricted to the allowlist, in
// allowlist order, with the relay's descriptions and schemas. The table is
// fetched once per process and again after an unknown-tool error.
func (f *mcpFacade) tools(ctx context.Context) ([]map[string]any, error) {
	f.tableMu.Lock()
	defer f.tableMu.Unlock()
	if f.table != nil {
		return f.table, nil
	}
	raw, err := f.relay(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var listed struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return nil, errors.New("relay returned an invalid tool table")
	}
	byName := map[string]map[string]any{}
	for _, tool := range listed.Tools {
		if name, _ := tool["name"].(string); name != "" {
			byName[name] = tool
		}
	}
	table := []map[string]any{}
	for _, name := range f.order {
		tool, ok := byName[name]
		if !ok {
			continue
		}
		entry := map[string]any{"name": name}
		for _, key := range []string{"title", "description", "inputSchema", "outputSchema", "annotations"} {
			if value, ok := tool[key]; ok && value != nil {
				entry[key] = value
			}
		}
		if _, ok := entry["inputSchema"]; !ok {
			entry["inputSchema"] = map[string]any{"type": "object"}
		}
		table = append(table, entry)
	}
	f.table = table
	return table, nil
}

func (f *mcpFacade) invalidate() {
	f.tableMu.Lock()
	f.table = nil
	f.tableMu.Unlock()
}

func (f *mcpFacade) call(ctx context.Context, params json.RawMessage) (any, *mcp.Error) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &mcp.Error{Code: mcp.CodeInvalidParams, Message: "params must be an object with name and arguments"}
		}
	}
	if p.Name == "" {
		return nil, &mcp.Error{Code: mcp.CodeInvalidParams, Message: "params.name is required"}
	}
	if !f.allowed[p.Name] {
		return nil, &mcp.Error{Code: mcp.CodeInvalidParams, Message: "Unknown tool: " + p.Name}
	}
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}
	result := f.forward(ctx, p.Name, p.Arguments)
	if template, ok := mcpUnsignedTemplate(result); ok {
		result = f.signAndResubmit(ctx, p.Name, p.Arguments, template)
	}
	return mcpBound(mcpPermissionError(result)), nil
}

// forward runs one tools/call on the relay and turns every failure into a
// tool result the model can read.
func (f *mcpFacade) forward(ctx context.Context, name string, arguments map[string]any) mcpToolResult {
	raw, err := f.relay(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		var relayErr *client.MCPError
		if errors.As(err, &relayErr) && relayErr.Code == mcp.CodeInvalidParams && strings.HasPrefix(relayErr.Message, "Unknown tool") {
			f.invalidate()
		}
		return mcpFailure(f.describe(err))
	}
	var result mcpToolResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return mcpFailure(mcpTransport + "the relay returned an invalid tool result")
	}
	if result.Content == nil {
		result.Content = []map[string]any{}
	}
	return result
}

// describe classifies a relay failure: HTTP 401 and 403 are permission
// errors, unreachable relays and 5xx answers are transport errors, and any
// other JSON-RPC error is reported with the relay's message.
func (f *mcpFacade) describe(err error) string {
	var relayErr *client.MCPError
	if !errors.As(err, &relayErr) {
		return mcpTransport + err.Error()
	}
	switch {
	case relayErr.Status == 401 || relayErr.Status == 403:
		return mcpPermission + relayErr.Message
	case relayErr.Status >= 500:
		return mcpTransport + relayErr.Error()
	default:
		return "relay: " + relayErr.Message
	}
}

// relay serializes calls to the relay and bounds each one.
func (f *mcpFacade) relay(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	f.relayMu.Lock()
	defer f.relayMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
	defer cancel()
	return f.client.MCP(ctx, method, params, f.info)
}

// mcpUnsignedTemplate recognizes a write tool's first answer: a successful
// result whose structuredContent.unsigned carries kind, tags and content.
func mcpUnsignedTemplate(result mcpToolResult) (nostr.Event, bool) {
	if result.IsError {
		return nostr.Event{}, false
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		return nostr.Event{}, false
	}
	unsigned, ok := structured["unsigned"].(map[string]any)
	if !ok {
		return nostr.Event{}, false
	}
	kind, ok := unsigned["kind"].(float64)
	if !ok || kind < 0 || kind != float64(int(kind)) {
		return nostr.Event{}, false
	}
	content, ok := unsigned["content"].(string)
	if !ok {
		return nostr.Event{}, false
	}
	rawTags, ok := unsigned["tags"].([]any)
	if !ok {
		return nostr.Event{}, false
	}
	tags := make([][]string, 0, len(rawTags))
	for _, rawTag := range rawTags {
		values, ok := rawTag.([]any)
		if !ok {
			return nostr.Event{}, false
		}
		tag := make([]string, 0, len(values))
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				return nostr.Event{}, false
			}
			tag = append(tag, text)
		}
		tags = append(tags, tag)
	}
	createdAt, _ := unsigned["created_at"].(float64)
	return nostr.Event{Kind: int(kind), CreatedAt: int64(createdAt), Tags: tags, Content: content}, true
}

// signAndResubmit signs the relay's template as returned, with no change to
// its kind, tags or content, and calls the same tool again with the signed
// event. The relay's publish result is what the model sees.
func (f *mcpFacade) signAndResubmit(ctx context.Context, name string, arguments map[string]any, template nostr.Event) mcpToolResult {
	if mcpAuthKinds[template.Kind] {
		return mcpFailure(fmt.Sprintf("refused to sign a kind %d authorization event returned by %s", template.Kind, name))
	}
	if template.CreatedAt == 0 {
		template.CreatedAt = time.Now().Unix()
	}
	if err := nostr.Sign(&template, f.secret); err != nil {
		return mcpFailure("sign: " + err.Error())
	}
	resubmit := make(map[string]any, len(arguments)+1)
	for k, v := range arguments {
		resubmit[k] = v
	}
	resubmit["event"] = template
	return f.forward(ctx, name, resubmit)
}

// mcpPermissionError marks relay refusals so the agent can ask for a grant.
func mcpPermissionError(result mcpToolResult) mcpToolResult {
	if !result.IsError {
		return result
	}
	if structured, ok := result.StructuredContent.(map[string]any); ok {
		if message, _ := structured["message"].(string); mcpRestricted(message) {
			return mcpFailureWith(mcpPermission+message, result.StructuredContent)
		}
	}
	for i, block := range result.Content {
		text, _ := block["text"].(string)
		if block["type"] == "text" && mcpRestricted(text) {
			result.Content[i]["text"] = mcpPermission + text
		}
	}
	return result
}

func mcpRestricted(message string) bool {
	return strings.HasPrefix(message, "restricted:") || strings.HasPrefix(message, "auth-required:")
}

// mcpBound caps each text block at mcpTextLimit bytes and says so.
func mcpBound(result mcpToolResult) mcpToolResult {
	for i, block := range result.Content {
		text, ok := block["text"].(string)
		if !ok || len(text) <= mcpTextLimit {
			continue
		}
		cut := mcpTextLimit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		result.Content[i]["text"] = text[:cut] + fmt.Sprintf("\n[truncated: %d of %d bytes shown]", cut, len(text))
	}
	return result
}

func mcpFailure(message string) mcpToolResult {
	return mcpFailureWith(message, nil)
}

func mcpFailureWith(message string, structured any) mcpToolResult {
	return mcpToolResult{Content: []map[string]any{{"type": "text", "text": message}}, StructuredContent: structured, IsError: true}
}

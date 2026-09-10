package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
)

// Server serves the MCP endpoint. Every request is authenticated, validated
// against its mirrored headers and routed independently; no state survives
// between requests.
type Server struct {
	// Tools is the tool table served by tools/list and tools/call.
	Tools *Registry
	// Info identifies this server in every result's _meta.
	Info Implementation
	// Instructions is optional guidance returned by server/discover.
	Instructions string
	// Authenticate resolves the caller from the request and its body. An
	// error answers 401 with Challenge in WWW-Authenticate.
	Authenticate func(r *http.Request, body []byte) (string, error)
	// Challenge is the WWW-Authenticate value sent with 401.
	Challenge string
	// Origin returns the origin this endpoint is served from. A request whose
	// Origin header names another origin is refused with 403.
	Origin func(r *http.Request) string
	// MaxBodyBytes bounds the request body. Zero means one megabyte.
	MaxBodyBytes int64
}

// Observation summarizes one handled request for telemetry. It never carries
// the caller's identity.
type Observation struct {
	Method  string
	Tool    string
	Outcome string
	Status  int
}

const (
	OutcomeOK           = "ok"
	OutcomeInvalid      = "invalid"
	OutcomeUnauthorized = "unauthorized"
	OutcomeError        = "error"
)

const (
	toolListTTL = 5 * 60 * 1000
	discoverTTL = 60 * 60 * 1000
	defaultBody = 1 << 20
)

// ServeHTTP handles one POST to the MCP endpoint.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.Handle(w, r) }

// Handle serves the request and reports what happened.
func (s *Server) Handle(w http.ResponseWriter, r *http.Request) Observation {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed: the MCP endpoint accepts POST", http.StatusMethodNotAllowed)
		return Observation{Outcome: OutcomeInvalid, Status: http.StatusMethodNotAllowed}
	}
	if origin := r.Header.Get("Origin"); origin != "" && s.Origin != nil && !strings.EqualFold(origin, s.Origin(r)) {
		return s.fail(w, nil, errorf(CodeInvalidRequest, http.StatusForbidden, "origin %s is not allowed", origin), Observation{})
	}
	limit := s.MaxBodyBytes
	if limit <= 0 {
		limit = defaultBody
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return s.fail(w, nil, errorf(CodeInvalidRequest, http.StatusRequestEntityTooLarge, "request body exceeds %d bytes", limit), Observation{})
		}
		return s.fail(w, nil, errorf(CodeInvalidRequest, http.StatusBadRequest, "request body could not be read"), Observation{})
	}
	actor := ""
	if s.Authenticate != nil {
		actor, err = s.Authenticate(r, body)
		if err != nil {
			if s.Challenge != "" {
				w.Header().Set("WWW-Authenticate", s.Challenge)
			}
			return s.fail(w, nil, errorf(CodeInvalidRequest, http.StatusUnauthorized, "%s", err.Error()), Observation{})
		}
	}
	if accept := r.Header.Get("Accept"); accept != "" && !accepts(accept, "application/json") {
		return s.fail(w, nil, errorf(CodeInvalidRequest, http.StatusNotAcceptable, "Accept must include application/json"), Observation{})
	}
	if media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); media != "application/json" {
		return s.fail(w, nil, errorf(CodeInvalidRequest, http.StatusUnsupportedMediaType, "Content-Type must be application/json"), Observation{})
	}
	var request Request
	if err := json.Unmarshal(body, &request); err != nil || !bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
		return s.fail(w, nil, errorf(CodeParse, http.StatusBadRequest, "body must be a single JSON-RPC request or notification"), Observation{})
	}
	obs := Observation{Method: request.Method}
	if request.JSONRPC != "2.0" || request.Method == "" {
		return s.fail(w, nil, errorf(CodeInvalidRequest, http.StatusBadRequest, "jsonrpc must be \"2.0\" and method is required"), obs)
	}
	if !request.Notification() && !validID(request.ID) {
		return s.fail(w, nil, errorf(CodeInvalidRequest, http.StatusBadRequest, "id must be a string or a number"), obs)
	}
	if request.Notification() {
		w.WriteHeader(http.StatusAccepted)
		obs.Outcome, obs.Status = OutcomeOK, http.StatusAccepted
		return obs
	}
	var p params
	if len(request.Params) > 0 {
		if err := json.Unmarshal(request.Params, &p); err != nil {
			return s.fail(w, request.ID, errorf(CodeInvalidParams, http.StatusBadRequest, "params must be an object"), obs)
		}
	}
	if err := validateHeaders(r, request, p); err != nil {
		return s.fail(w, request.ID, err, obs)
	}
	var result any
	var rpcErr *Error
	switch request.Method {
	case "server/discover":
		result = s.discover()
	case "tools/list":
		result = s.list()
	case "tools/call":
		obs.Tool = p.Name
		var tool Result
		tool, rpcErr = s.call(r.Context(), r, actor, p)
		if rpcErr == nil {
			result = s.result(map[string]any{"content": tool.Content, "structuredContent": tool.StructuredContent, "isError": tool.IsError})
			if tool.IsError {
				obs.Outcome = OutcomeInvalid
			}
		}
	default:
		rpcErr = errorf(CodeMethodNotFound, http.StatusNotFound, "method %s is not supported", request.Method)
	}
	if rpcErr != nil {
		return s.fail(w, request.ID, rpcErr, obs)
	}
	if obs.Outcome == "" {
		obs.Outcome = OutcomeOK
	}
	obs.Status = http.StatusOK
	write(w, http.StatusOK, Response{JSONRPC: "2.0", ID: request.ID, Result: result})
	return obs
}

func (s *Server) fail(w http.ResponseWriter, id json.RawMessage, err *Error, obs Observation) Observation {
	status := err.Status
	if status == 0 {
		status = http.StatusBadRequest
	}
	obs.Status = status
	switch {
	case status == http.StatusUnauthorized:
		obs.Outcome = OutcomeUnauthorized
	case status >= 500:
		obs.Outcome = OutcomeError
	default:
		obs.Outcome = OutcomeInvalid
	}
	write(w, status, Response{JSONRPC: "2.0", ID: id, Error: err})
	return obs
}

// validateHeaders enforces the request metadata rules of the Streamable HTTP
// binding: the protocol version in the header and in _meta must agree and be
// supported, Mcp-Method must match the method, and Mcp-Name must match the
// tool name of a tools/call request.
func validateHeaders(r *http.Request, request Request, p params) *Error {
	version := r.Header.Get(HeaderProtocolVersion)
	if version == "" {
		return errorf(CodeHeaderMismatch, http.StatusBadRequest, "missing %s header; this server speaks %s", HeaderProtocolVersion, strings.Join(SupportedVersions, ", "))
	}
	if !supported(version) {
		return &Error{Code: CodeUnsupportedProtocolVersion, Status: http.StatusBadRequest, Message: "Unsupported protocol version", Data: map[string]any{"supported": SupportedVersions, "requested": version}}
	}
	metaVersion, ok := metaString(p.Meta, MetaProtocolVersion)
	if !ok {
		return errorf(CodeInvalidParams, http.StatusBadRequest, "params._meta[%q] is required", MetaProtocolVersion)
	}
	if metaVersion != version {
		return errorf(CodeHeaderMismatch, http.StatusBadRequest, "Header mismatch: %s header value '%s' does not match body value '%s'", HeaderProtocolVersion, version, metaVersion)
	}
	if capabilities, present := p.Meta[MetaClientCapabilities]; !present || !bytes.HasPrefix(bytes.TrimSpace(capabilities), []byte("{")) {
		return errorf(CodeInvalidParams, http.StatusBadRequest, "params._meta[%q] is required; send {} when the client has none", MetaClientCapabilities)
	}
	method := r.Header.Get(HeaderMethod)
	if method == "" {
		return errorf(CodeHeaderMismatch, http.StatusBadRequest, "missing %s header", HeaderMethod)
	}
	if method != request.Method {
		return errorf(CodeHeaderMismatch, http.StatusBadRequest, "Header mismatch: %s header value '%s' does not match body value '%s'", HeaderMethod, method, request.Method)
	}
	source := ""
	switch request.Method {
	case "tools/call", "prompts/get":
		source = p.Name
	case "resources/read":
		source = p.URI
	default:
		return nil
	}
	raw := r.Header.Get(HeaderName)
	if raw == "" {
		return errorf(CodeHeaderMismatch, http.StatusBadRequest, "missing %s header", HeaderName)
	}
	name, err := DecodeHeaderValue(raw)
	if err != nil {
		return errorf(CodeHeaderMismatch, http.StatusBadRequest, "Header mismatch: %s %s", HeaderName, err.Error())
	}
	if name != source {
		return errorf(CodeHeaderMismatch, http.StatusBadRequest, "Header mismatch: %s header value '%s' does not match body value '%s'", HeaderName, name, source)
	}
	return nil
}

func (s *Server) discover() map[string]any {
	return s.result(map[string]any{
		"supportedVersions": SupportedVersions,
		"capabilities":      map[string]any{"tools": map[string]any{}},
		"instructions":      s.Instructions,
		"ttlMs":             discoverTTL,
		"cacheScope":        "private",
	})
}

func (s *Server) list() map[string]any {
	tools := s.Tools.Tools()
	if tools == nil {
		tools = []Tool{}
	}
	return s.result(map[string]any{"tools": tools, "ttlMs": toolListTTL, "cacheScope": "private"})
}

func (s *Server) call(ctx context.Context, r *http.Request, actor string, p params) (Result, *Error) {
	if p.Name == "" {
		return Result{}, errorf(CodeInvalidParams, http.StatusBadRequest, "params.name is required")
	}
	tool, ok := s.Tools.Lookup(p.Name)
	if !ok {
		return Result{}, errorf(CodeInvalidParams, http.StatusBadRequest, "Unknown tool: %s", p.Name)
	}
	arguments := map[string]any{}
	if len(p.Arguments) > 0 && string(p.Arguments) != "null" {
		if err := json.Unmarshal(p.Arguments, &arguments); err != nil {
			return Result{}, errorf(CodeInvalidParams, http.StatusBadRequest, "params.arguments must be an object")
		}
	}
	if err := Validate(tool.InputSchema, arguments); err != nil {
		return Failure(err.Error(), map[string]any{"inputSchema": tool.InputSchema}).filled(), nil
	}
	result, err := tool.Handler(ctx, Call{Actor: actor, Name: tool.Name, Arguments: arguments, Request: r})
	if err != nil {
		return Failure(err.Error(), nil).filled(), nil
	}
	return result.filled(), nil
}

func (s *Server) result(fields map[string]any) map[string]any {
	fields["resultType"] = "complete"
	if s.Info.Name != "" {
		fields["_meta"] = map[string]any{MetaServerInfo: s.Info}
	}
	return fields
}

func metaString(meta map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := meta[key]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func validID(id json.RawMessage) bool {
	trimmed := bytes.TrimSpace(id)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return false
	}
	return trimmed[0] == '"' || trimmed[0] == '-' || (trimmed[0] >= '0' && trimmed[0] <= '9')
}

func accepts(header, media string) bool {
	for _, part := range strings.Split(header, ",") {
		value := strings.TrimSpace(strings.Split(part, ";")[0])
		if value == media || value == "*/*" || value == strings.Split(media, "/")[0]+"/*" {
			return true
		}
	}
	return false
}

func write(w http.ResponseWriter, status int, response Response) {
	raw, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "could not encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

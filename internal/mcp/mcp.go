// Package mcp implements the Model Context Protocol, revision 2026-07-28, in
// the stateless form of its Streamable HTTP binding: one endpoint that
// accepts POST, no sessions, no initialize handshake, and every request
// carrying its protocol version in both the headers and the body.
package mcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Version is the protocol revision this package implements.
const Version = "2026-07-28"

// SupportedVersions lists the protocol revisions the server accepts. Older
// revisions negotiated a session with an initialize handshake and are not
// served.
var SupportedVersions = []string{Version}

// Header names mirrored from the JSON-RPC body so intermediaries can route
// without parsing it. Comparison of names is case-insensitive; values are
// case-sensitive.
const (
	HeaderProtocolVersion = "MCP-Protocol-Version"
	HeaderMethod          = "Mcp-Method"
	HeaderName            = "Mcp-Name"
)

// Reserved _meta keys carried on every request and result.
const (
	MetaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	MetaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	MetaClientInfo         = "io.modelcontextprotocol/clientInfo"
	MetaServerInfo         = "io.modelcontextprotocol/serverInfo"
)

// JSON-RPC error codes. The -32020 to -32099 range is reserved for the MCP
// specification and only the defined codes may be emitted.
const (
	CodeParse                      = -32700
	CodeInvalidRequest             = -32600
	CodeMethodNotFound             = -32601
	CodeInvalidParams              = -32602
	CodeInternal                   = -32603
	CodeHeaderMismatch             = -32020
	CodeMissingClientCapability    = -32021
	CodeUnsupportedProtocolVersion = -32022
)

// Error is a JSON-RPC error object. Status is the HTTP status the transport
// answers with and is never serialized.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
	Status  int    `json:"-"`
}

func (e *Error) Error() string { return fmt.Sprintf("mcp: %d %s", e.Code, e.Message) }

// Request is an incoming JSON-RPC request or notification. ID is nil for a
// notification.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Notification reports whether the message carries no id and therefore
// expects no response.
func (r Request) Notification() bool { return len(r.ID) == 0 }

// Response is an outgoing JSON-RPC result or error.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Implementation names a client or server.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// params is the subset of request parameters the transport inspects.
type params struct {
	Meta      map[string]json.RawMessage `json:"_meta"`
	Name      string                     `json:"name"`
	URI       string                     `json:"uri"`
	Cursor    string                     `json:"cursor"`
	Arguments json.RawMessage            `json:"arguments"`
}

const (
	sentinelPrefix = "=?base64?"
	sentinelSuffix = "?="
)

// EncodeHeaderValue renders a body value for the Mcp-Name or Mcp-Param
// headers. Values outside the safe ASCII set, with surrounding whitespace, or
// that already look like the sentinel are Base64 encoded.
func EncodeHeaderValue(value string) string {
	if headerSafe(value) && !sentinel(value) {
		return value
	}
	return sentinelPrefix + base64.StdEncoding.EncodeToString([]byte(value)) + sentinelSuffix
}

// DecodeHeaderValue reverses EncodeHeaderValue. Plain values must consist of
// visible ASCII, space and horizontal tab.
func DecodeHeaderValue(value string) (string, error) {
	if sentinel(value) {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(value, sentinelPrefix), sentinelSuffix))
		if err != nil {
			return "", fmt.Errorf("invalid Base64 header value")
		}
		return string(raw), nil
	}
	if !headerSafe(value) {
		return "", fmt.Errorf("header value contains invalid characters")
	}
	return value, nil
}

func sentinel(value string) bool {
	return strings.HasPrefix(value, sentinelPrefix) && strings.HasSuffix(value, sentinelSuffix) && len(value) >= len(sentinelPrefix)+len(sentinelSuffix)
}

func headerSafe(value string) bool {
	if value != strings.TrimSpace(value) {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == ' ' || c == '\t' || (c >= 0x21 && c <= 0x7e) {
			continue
		}
		return false
	}
	return true
}

func errorf(code, status int, format string, args ...any) *Error {
	return &Error{Code: code, Status: status, Message: fmt.Sprintf(format, args...)}
}

func supported(version string) bool {
	for _, v := range SupportedVersions {
		if v == version {
			return true
		}
	}
	return false
}

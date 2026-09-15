package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/FelineStateMachine/tinyrelay/internal/mcp"
)

// MCPError is a JSON-RPC error answered by the relay's MCP endpoint together
// with the HTTP status it arrived with. Failures to reach the relay at all
// are returned as ordinary errors instead.
type MCPError struct {
	Status  int
	Code    int
	Message string
	Data    any
}

func (e *MCPError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("relay HTTP %d, JSON-RPC %d: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("relay HTTP %d: %s", e.Status, e.Message)
}

var mcpRequestID atomic.Int64

// MCP posts one JSON-RPC request to the relay's stateless MCP endpoint and
// returns the raw result. Each call carries a fresh NIP-98 proof bound to
// the URL and body, the mirrored Mcp-* headers and the reserved _meta
// fields. The endpoint is /mcp under the client's base URL, so a tenant
// served under a path prefix is reached at /r/<name>/mcp.
func (c *Client) MCP(ctx context.Context, method string, params map[string]any, info mcp.Implementation) (json.RawMessage, error) {
	if method == "" {
		return nil, errors.New("method is required")
	}
	merged := make(map[string]any, len(params)+1)
	for k, v := range params {
		merged[k] = v
	}
	merged["_meta"] = map[string]any{
		mcp.MetaProtocolVersion:    mcp.Version,
		mcp.MetaClientCapabilities: map[string]any{},
		mcp.MetaClientInfo:         info,
	}
	body, err := json.Marshal(mcp.Request{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprint(mcpRequestID.Add(1))), Method: method, Params: mustJSON(merged)})
	if err != nil {
		return nil, err
	}
	u, err := c.requestURL("/mcp")
	if err != nil {
		return nil, err
	}
	proof, err := c.proof(http.MethodPost, u.String(), body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", proof)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(mcp.HeaderProtocolVersion, mcp.Version)
	req.Header.Set(mcp.HeaderMethod, method)
	if method == "tools/call" {
		name, _ := params["name"].(string)
		req.Header.Set(mcp.HeaderName, mcp.EncodeHeaderValue(name))
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDownload {
		return nil, errors.New("response exceeds 32 MiB")
	}
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  *mcp.Error      `json:"error"`
	}
	decoded := json.Unmarshal(data, &response) == nil
	if resp.StatusCode/100 != 2 {
		if decoded && response.Error != nil {
			return nil, &MCPError{Status: resp.StatusCode, Code: response.Error.Code, Message: response.Error.Message, Data: response.Error.Data}
		}
		message := strings.TrimSpace(string(data))
		if message == "" {
			message = resp.Status
		}
		return nil, &MCPError{Status: resp.StatusCode, Message: message}
	}
	if !decoded {
		return nil, errors.New("relay returned an invalid JSON-RPC response")
	}
	if response.Error != nil {
		return nil, &MCPError{Status: resp.StatusCode, Code: response.Error.Code, Message: response.Error.Message, Data: response.Error.Data}
	}
	if len(response.Result) == 0 {
		return nil, errors.New("relay returned a JSON-RPC response without a result")
	}
	return response.Result, nil
}

func mustJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage("{}")
	}
	return raw
}

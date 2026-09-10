// Package mcp serves the Model Context Protocol revision 2026-07-28 through
// its stateless Streamable HTTP binding.
//
// Server accepts one JSON-RPC request or notification per POST. Protocol
// version, method, tool name and selected resource name are mirrored in HTTP
// headers and checked against the JSON body. Registry keeps tools in
// registration order, validates their JSON Schema subset, and invokes each
// Handler with an authenticated actor and request context. Tool failures are
// returned as execution results; transport and validation failures use
// JSON-RPC errors.
package mcp

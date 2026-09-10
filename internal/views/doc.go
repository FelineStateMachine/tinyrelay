// Package views parses fenced code blocks and connects them to custom view
// renderers.
//
// Blocks preserves source order and normalizes CRLF line endings. Hash names
// an artifact by its language and source, while Path gives its relay URL.
// Figure emits accessible markup with the rendered image and an inline source
// fallback. Renderer maps each lowercased language to the first registered
// view that supports it.
package views

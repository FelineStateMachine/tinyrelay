// Package templates contains the built-in relay policy and connection catalog.
//
// Templates describe a named starting policy, event kind limits, retention,
// source settings and recommended connections. Connections describe the app,
// visibility, inputs, links and copyable values shown by the relay UI. The
// catalog functions return copies so callers can customize results without
// changing the built-in definitions. ApplyTemplate starts with policy defaults
// for an owner, applies the template patch, then applies kind and retention
// limits.
package templates

// Package webui renders the relay's HTML pages and browser support modules.
//
// App owns the HTTP surface, templates, page data and browser assets. It asks
// Backend for relay identity, policy and method results, and uses ActorResolver
// to associate a request with the authenticated public key. The daemon owns
// relay policy and relay writes.
//
// Pages use a shared shell in page.html. The shell supplies the navigation rail,
// content column, contextual panel and command footer. Each page template
// supplies one content section and remains readable when scripts are disabled.
// JavaScript modules progressively add signing, navigation, forms, components,
// room streams, file transfers and WebMCP controls.
package webui

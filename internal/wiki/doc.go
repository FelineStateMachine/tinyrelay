// Package wiki implements storage-independent NIP-54 article behavior.
//
// Normalize converts a title to an article d tag. Links discovers wikilinks,
// reference links and nostr: targets in Djot content. RenderHTML supports the
// article syntax used by the relay, escapes text and unsafe link targets, and
// returns template HTML for server-side insertion. RenderHTMLWith accepts a
// fenced-block callback so custom views can replace selected code blocks.
package wiki

package event

import "github.com/FelineStateMachine/tinyrelay/protocol/nostr"

// Filter is the public subscription and query value.
type Filter = nostr.Filter

func ParseFilter(raw []byte) (Filter, error) { return nostr.ParseFilter(raw) }
func SearchTerms(query string) []string      { return nostr.SearchTerms(query) }

// Matches preserves the host's search visibility rule for existing consumers.
// Generic field, tag and content matching lives in protocol/nostr.
func Matches(f Filter, e Event) bool {
	if IsPrivate(e.Kind) && len(nostr.SearchTerms(f.Search)) > 0 {
		return false
	}
	return nostr.Matches(f, e)
}

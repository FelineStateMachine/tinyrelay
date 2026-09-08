package webui

import (
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

func TestNIP19PointersMatchReferenceEncodings(t *testing.T) {
	key := strings.Repeat("a", 64)
	if got := nprofile(key, "wss://relay.example"); got != "nprofile1qqs242424242424242424242424242424242424242424242424242spzdmhxue69uhhyetvv9ujuetcv9khqmr9nxcavj" {
		t.Fatalf("nprofile = %s", got)
	}
	if got := naddr(39000, key, "_", "wss://relay.example"); got != "naddr1qqq47qgnwaehxw309aex2mrp0yhx27rpd4cxcegzyz424242424242424242424242424242424242424242424242425qcyqqqfskquzv50g" {
		t.Fatalf("naddr = %s", got)
	}
	if got := bech32("npub", mustHex(key)); got != "npub1424242424242424242424242424242424242424242424242424qamrcaj" {
		t.Fatalf("npub = %s", got)
	}
}

func TestConnectionsExpandPlaceholdersAndHideUserLinksForGuests(t *testing.T) {
	data := PageData{URL: "https://relay.example", Identity: strings.Repeat("a", 64), Policy: policy.Defaults(strings.Repeat("b", 64))}
	rows := []any{map[string]any{"template": "repos", "title": "Repos", "visibility": "public", "links": []any{
		map[string]any{"label": "Open", "href": "https://gitworkshop.dev/relay/{relay:host|enc}"},
		map[string]any{"Label": "Copy clone command", "Copy": "git clone '{relay:web}/{user:npub}/{input:repo}.git'"},
	}}, map[string]any{"template": "group", "title": "Group", "qr": "nostr:{relay:naddr}", "links": []any{map[string]any{"label": "Open", "href": "https://app.flotilla.social/spaces/{relay:host|enc}"}}}}
	cards := expandConnections(rows, data)
	repos := valueMap(cards[0])
	links := browseRows(repos["links"])
	if len(links) != 1 || valueMap(links[0])["href"] != "https://gitworkshop.dev/relay/relay.example" || repos["hidden"] != 1 {
		t.Fatalf("guest repos card = %v", repos)
	}
	group := valueMap(cards[1])
	if !strings.HasPrefix(plainString(group["qr"]), "nostr:naddr1") {
		t.Fatalf("group qr = %v", group["qr"])
	}
	data.Actor = strings.Repeat("c", 64)
	cards = expandConnections(rows, data)
	links = browseRows(valueMap(cards[0])["links"])
	if len(links) != 2 || !strings.Contains(plainString(valueMap(links[1])["copy"]), "/npub1enxvenxv") || !strings.Contains(plainString(valueMap(links[1])["copy"]), "{input:repo}") {
		t.Fatalf("member repos links = %v", links)
	}
}

func mustHex(value string) []byte {
	out := make([]byte, len(value)/2)
	for i := range out {
		out[i] = hexNibble(value[2*i])<<4 | hexNibble(value[2*i+1])
	}
	return out
}

func hexNibble(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return 0
}

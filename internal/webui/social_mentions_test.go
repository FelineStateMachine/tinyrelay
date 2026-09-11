package webui

import (
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
)

func TestSocialNostrLinksRewritesKnownReferences(t *testing.T) {
	pubkey := "5ac640e5df8f7945381c31f435288ba3f587fbd68efb27abaf941792f9c36369"
	id := "0f8fc45a6e3cd2d022cf68f3ea0f413665a6d92c3d783e3ab6c2bc324432b5af"
	pubkeyBytes, _ := hex.DecodeString(pubkey)
	idBytes, _ := hex.DecodeString(id)
	kind := make([]byte, 4)
	binary.BigEndian.PutUint32(kind, 30023)
	naddrData := append([]byte{0, 5}, []byte("hello")...)
	naddrData = append(naddrData, append([]byte{2, 32}, pubkeyBytes...)...)
	naddrData = append(naddrData, append([]byte{3, 4}, kind...)...)
	refs := []struct {
		name string
		ref  string
		want string
	}{
		{"profile", "nostr:" + bech32("npub", pubkeyBytes), `<a href="/social?author=` + pubkey + `"><nostr-name pubkey="` + pubkey + `">` + shortID(pubkey) + `</nostr-name></a>`},
		{"event", "nostr:" + bech32("note", idBytes), `<a href="/e/` + id + `">Quoted post</a>`},
		{"article", "nostr:" + bech32("naddr", naddrData), `<a href="/a/30023:` + pubkey + `:hello">Article</a>`},
	}
	for _, test := range refs {
		rendered := `<p>See <a href="/open?target=` + test.ref + `">` + test.ref + `</a> today.</p>`
		if got := socialNostrLinks(rendered); !strings.Contains(got, test.want) {
			t.Errorf("%s: got %q, want %q", test.name, got, test.want)
		}
	}
}

func TestSocialNostrLinksLeavesUnknownMarkupAndCodeAlone(t *testing.T) {
	input := `<p><a href="/open?target=nostr%3Ainvalid">nostr:invalid</a> <code>nostr:invalid</code> <a href="https://example.com">nostr:invalid</a></p>`
	if got := socialNostrLinks(input); got != input {
		t.Fatalf("got %q, want unchanged markup", got)
	}
}

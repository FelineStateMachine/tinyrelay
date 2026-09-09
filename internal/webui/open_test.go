package webui

import (
	"encoding/binary"
	"encoding/hex"
	"testing"
)

func TestNostrTargetPathDecodesNIP19Links(t *testing.T) {
	pubkey := "5ac640e5df8f7945381c31f435288ba3f587fbd68efb27abaf941792f9c36369"
	raw, _ := hex.DecodeString(pubkey)
	id := "0f8fc45a6e3cd2d022cf68f3ea0f413665a6d92c3d783e3ab6c2bc324432b5af"
	idRaw, _ := hex.DecodeString(id)
	kind := make([]byte, 4)
	binary.BigEndian.PutUint32(kind, 30023)
	naddr := append(append(append([]byte{0, 5}, []byte("hello")...), append([]byte{2, 32}, raw...)...), append([]byte{3, 4}, kind...)...)
	cases := map[string]string{
		"web+nostr:npub1ttrypewl3au52wqux86r22yt506c077k3maj02a0jste97wrvd5sjfutc2": "/search?author=" + pubkey,
		"nostr:" + bech32("note", idRaw):                                            "/e/" + id,
		bech32("nevent", append([]byte{0, 32}, idRaw...)):                           "/e/" + id,
		"web+nostr://" + bech32("nprofile", append([]byte{0, 32}, raw...)):          "/search?author=" + pubkey,
		bech32("naddr", naddr):                                                      "/a/30023:" + pubkey + ":hello",
		id:                                                                          "/e/" + id,
	}
	for target, want := range cases {
		if got, ok := nostrTargetPath(target); !ok || got != want {
			t.Fatalf("%s: got %q ok=%v, want %q", target, got, ok, want)
		}
	}
	for _, bad := range []string{"", "web+nostr:npub1ttrypewl3au52wqux86r22yt506c077k3maj02a0jste97wrvd5sjfutc3", "nostr:javascript:alert(1)", "nsec1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqs4g4gaj"} {
		if _, ok := nostrTargetPath(bad); ok {
			t.Fatalf("%q accepted", bad)
		}
	}
}

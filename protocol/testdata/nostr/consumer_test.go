package consumer_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
	"github.com/FelineStateMachine/tinyrelay/tinyclient"
	"github.com/FelineStateMachine/tinyrelay/tinygit"
)

func TestPublicEventRoundTripAndConsumerTypes(t *testing.T) {
	e := nostr.Event{CreatedAt: 1, Kind: 1, Content: "hello <world>\u2028", Tags: [][]string{{"t", "go"}}}
	if err := nostr.Sign(&e, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	raw, err := nostr.Canonical(e)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := nostr.Parse(raw)
	if err != nil || !reflect.DeepEqual(parsed, e) {
		t.Fatalf("round trip: %#v, %v", parsed, err)
	}
	var gitEvent tinygit.Event = parsed
	var message tinyclient.RoomMessage = gitEvent
	if reflect.TypeOf(message).PkgPath() != "github.com/FelineStateMachine/tinyrelay/protocol/nostr" {
		t.Fatalf("public event is owned by %s", reflect.TypeOf(message).PkgPath())
	}
	f, err := nostr.ParseFilter([]byte(`{"#t":["go"],"since":1,"until":1}`))
	if err != nil || !nostr.Matches(f, message) {
		t.Fatalf("filter round trip: %#v, %v", f, err)
	}
	// Matching is not host visibility policy, even for private feature kinds.
	message.Kind, message.Content = 30390, "hello"
	if !nostr.Matches(nostr.Filter{Search: "hello"}, message) {
		t.Fatal("generic matcher applied host privacy policy")
	}
}

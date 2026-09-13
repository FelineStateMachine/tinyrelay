package event

import (
	"reflect"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
)

func TestCompatibilityPreservesPublicTypeIdentity(t *testing.T) {
	if reflect.TypeOf(Event{}).PkgPath() != reflect.TypeOf(nostr.Event{}).PkgPath() || reflect.TypeOf(Filter{}).PkgPath() != reflect.TypeOf(nostr.Filter{}).PkgPath() {
		t.Fatal("compatibility values must alias the public types")
	}
}

func TestPrivateKindClassificationAndSearchCompatibility(t *testing.T) {
	if !IsPrivate(4) || IsPrivate(KIND_DM) {
		t.Fatal("kind classification mismatch")
	}
	for _, kind := range []int{4, 13, KIND_WRAP, 21059, KIND_NOSTR_CONNECT, KIND_PUSH_REGISTRATION} {
		e := Event{Kind: kind, Content: "hello world"}
		if Matches(Filter{Search: "hello"}, e) {
			t.Errorf("private kind %d matched content search", kind)
		}
		if !Matches(Filter{Search: "lang:en"}, e) || !Matches(Filter{}, e) {
			t.Errorf("private kind %d changed non-content matching", kind)
		}
	}
	if !Matches(Filter{Search: "HELLO"}, Event{Kind: 1, Content: "hello world"}) {
		t.Fatal("public content no longer matches")
	}
}

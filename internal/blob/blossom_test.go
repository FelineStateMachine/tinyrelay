package blob

import (
	"reflect"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestBlossomURIAndFallbacks(t *testing.T) {
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	author := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	u, err := ParseBlossomURI("BLOSSOM:" + hash + ".png?xs=cdn.example&xs=https%3A%2F%2Fb.example&as=" + author + "&as=" + hash + "&sz=42")
	if err != nil {
		t.Fatal(err)
	}
	if u.Hash != hash || u.Extension != "png" || u.Size != 42 || len(u.Servers) != 2 || len(u.Authors) != 2 {
		t.Fatalf("parsed URI = %#v", u)
	}
	if got := BlobHashFromURL("https://broken/" + hash + ".png"); got != hash {
		t.Fatalf("hash from URL = %q", got)
	}
	want := []string{"https://cdn.example/" + hash + ".png", "http://cdn.example/" + hash + ".png", "https://b.example/" + hash + ".png"}
	if got := FallbackURLs(u.Servers, hash, "png"); !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback URLs = %#v", got)
	}
}

func TestBlossomServerListPreservesOrder(t *testing.T) {
	hashless := event.Event{Kind: 10063, Tags: [][]string{{"server", "https://a.example/"}, {"server", "https://b.example"}, {"server", "https://a.example"}}}
	got, err := BlossomServerList(hashless)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://a.example", "https://b.example"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("servers = %#v", got)
	}
}

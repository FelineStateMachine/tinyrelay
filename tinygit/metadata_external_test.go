package tinygit_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/tinygit"
)

func TestParseMetadataRejectsInvalidShapes(t *testing.T) {
	oid := strings.Repeat("a", 40)
	for name, tags := range map[string][][]string{
		"missing identifier": {},
		"unsafe identifier":  {{"d", "../demo"}},
		"invalid ref":        {{"d", "demo"}, {"refs/heads/a..b", oid}},
		"invalid oid":        {{"d", "demo"}, {"refs/heads/main", "not-a-sha1"}},
		"uppercase oid":      {{"d", "demo"}, {"refs/heads/main", strings.Repeat("A", 40)}},
		"zero oid":           {{"d", "demo"}, {"refs/heads/main", strings.Repeat("0", 40)}},
		"empty oid":          {{"d", "demo"}, {"refs/heads/main", ""}},
		"duplicate ref":      {{"d", "demo"}, {"refs/heads/main", oid}, {"refs/heads/main", oid}},
		"duplicate head":     {{"d", "demo"}, {"HEAD", "ref: refs/heads/main"}, {"HEAD", "ref: refs/heads/main"}},
		"short head":         {{"d", "demo"}, {"HEAD"}},
		"long head":          {{"d", "demo"}, {"HEAD", "ref: refs/heads/main", "extra"}},
		"detached head":      {{"d", "demo"}, {"HEAD", oid}},
		"nonbranch head":     {{"d", "demo"}, {"HEAD", "ref: refs/nostr/topic"}},
		"invalid head ref":   {{"d", "demo"}, {"HEAD", "ref: refs/heads/bad..name"}},
	} {
		t.Run(name, func(t *testing.T) {
			m, err := tinygit.ParseMetadata(tinygit.Event{Kind: 30618, Tags: tags})
			if err == nil || !strings.HasPrefix(err.Error(), "invalid:") || !reflect.DeepEqual(m, tinygit.Metadata{}) {
				t.Fatalf("invalid shape: metadata=%#v err=%v", m, err)
			}
		})
	}
	if _, err := tinygit.ParseMetadata(tinygit.Event{Kind: 1, Tags: [][]string{{"d", "demo"}}}); err == nil || !strings.HasPrefix(err.Error(), "unsupported:") {
		t.Fatalf("unsupported kind: %v", err)
	}
}

func TestParseMetadataPreservesAnnouncementClaims(t *testing.T) {
	e := tinygit.Event{Kind: 30617, PubKey: "author", Tags: [][]string{
		{}, {"d", "demo"}, {"d", "ignored-second-identifier"},
		{"private", "true"}, {"clone", "https://one", "https://two"}, {"clone", "https://three"},
		{"relays", "wss://one", "wss://two"}, {"maintainers", "claimed-key", "another-key"},
		{"unknown"}, {"HEAD", "not-state"},
	}}
	m, err := tinygit.ParseMetadata(e)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Private || m.Identifier != "demo" || m.Author != "author" || m.Head != "" || len(m.Refs) != 0 ||
		!reflect.DeepEqual(m.Clone, []string{"https://one", "https://two", "https://three"}) ||
		!reflect.DeepEqual(m.Relays, []string{"wss://one", "wss://two"}) ||
		!reflect.DeepEqual(m.Maintainers, []string{"claimed-key", "another-key"}) {
		t.Fatalf("announcement claims = %#v", m)
	}
	m.Clone[0], m.Relays[0], m.Maintainers[0] = "changed", "changed", "changed"
	again, err := tinygit.ParseMetadata(e)
	if err != nil || again.Clone[0] != "https://one" || again.Relays[0] != "wss://one" || again.Maintainers[0] != "claimed-key" {
		t.Fatalf("metadata aliases input tags: %#v, %v", again, err)
	}
}

func TestParseMetadataWithoutSignatureOrHost(t *testing.T) {
	// Shape parsing is useful before signing. These are claims, not authority.
	e := tinygit.Event{Kind: 30618, PubKey: "unsigned-author", ID: "unverified-id", Tags: [][]string{
		{"d", "demo"}, {"HEAD", "ref: refs/heads/unborn"},
		{"refs/heads/main", strings.Repeat("a", 40), "extension"},
		{"refs/tags/v1", strings.Repeat("b", 40)}, {"unknown", "ignored"},
	}}
	m, err := tinygit.ParseMetadata(e)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != e.Kind || m.Author != e.PubKey || m.EventID != e.ID || m.Identifier != "demo" || m.Head != "ref: refs/heads/unborn" {
		t.Fatalf("metadata claims = %#v", m)
	}
	if !reflect.DeepEqual(m.Refs, map[string]string{"refs/heads/main": strings.Repeat("a", 40), "refs/tags/v1": strings.Repeat("b", 40)}) {
		t.Fatalf("refs = %#v", m.Refs)
	}
}

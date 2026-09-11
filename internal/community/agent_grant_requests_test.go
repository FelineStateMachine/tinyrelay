package community

import (
	"reflect"
	"testing"
)

func TestGrantRequestTagsPatchOnlyRequestedEntries(t *testing.T) {
	old := [][]string{
		{"d", "agent"}, {"p", "agent"}, {"expiration", "200"},
		{"k", "1"}, {"k", "9"}, {"room", "alpha"}, {"room", "beta"},
		{"repo", "owner:one:read"}, {"repo", "owner:two:maintain"},
		{"grant-request", "old"}, {"grant-base", "old"}, {"e", "old", "", "grant-request"},
	}
	want := [][]string{
		{"d", "agent"}, {"p", "agent"}, {"expiration", "200"},
		{"k", "1"}, {"k", "9"}, {"room", "alpha"}, {"room", "beta"},
		{"repo", "owner:one:read"}, {"repo", "owner:two:maintain"},
		{"k", "11"},
	}
	got := grantRequestTags(old, AgentGrantChanges{Kinds: []int{11}, Rooms: []string{"alpha"}})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("patch tags = %#v, want %#v", got, want)
	}
}

func TestGrantRequestPatchKeepsOtherRepositoriesAndSites(t *testing.T) {
	old := [][]string{{"repo", "owner:one:read"}, {"repo", "owner:two:maintain"}, {"sites", "one", "encrypted"}, {"sites", "two", "ttl=7"}}
	changed := grantRequestTags(old, AgentGrantChanges{Repos: []AgentRepo{{Owner: "owner", Identifier: "one", Level: "maintain"}}, Sites: []AgentSite{{Label: "one", TTLDays: 3}}})
	want := [][]string{{"repo", "owner:two:maintain"}, {"sites", "two", "ttl=7"}, {"repo", "owner:one:maintain"}, {"sites", "one", "ttl=3"}}
	if !reflect.DeepEqual(changed, want) {
		t.Fatalf("patch changed unrelated entries: %v", changed)
	}
}

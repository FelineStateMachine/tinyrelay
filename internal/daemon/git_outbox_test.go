package daemon

import (
	"context"
	"fmt"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

type gitOutboxTransport struct {
	queries map[string][]event.Event
	errors  map[string]error
}

func (t gitOutboxTransport) Query(_ context.Context, target string, _ event.Filter) ([]event.Event, error) {
	if err := t.errors[target]; err != nil {
		return nil, err
	}
	if items := t.queries[target]; items != nil {
		return items, nil
	}
	return nil, nil
}

func TestGitOutboxFilterIncludesProfilesRelayListsAndRepositoryRelayLists(t *testing.T) {
	key := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	filter := gitOutboxFilter(key, 10002)
	if len(filter.Authors) != 1 || filter.Authors[0] != key {
		t.Fatalf("authors = %#v", filter.Authors)
	}
	if !containsGitKind(filter.Kinds, 10002) || len(filter.Kinds) != 1 {
		t.Fatalf("kinds = %#v", filter.Kinds)
	}
	if filter.Limit == nil || *filter.Limit != 1 {
		t.Fatalf("limit = %#v", filter.Limit)
	}
}

func TestGitOutboxAuthorsAndRelaysAreDeduplicatedAndBounded(t *testing.T) {
	key := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	authors := make([]string, 0, gitOutboxAuthorLimit+2)
	for i := 0; i < gitOutboxAuthorLimit+2; i++ {
		authors = append(authors, key)
	}
	if got := boundedGitAuthors(authors); len(got) != 1 {
		t.Fatalf("authors = %#v", got)
	}
	relays := boundedGitRelays([]string{"https://Relay.EXAMPLE/", "wss://relay.example"}, false, nil)
	if len(relays) != 1 || relays[0] != "wss://relay.example" {
		t.Fatalf("relays = %#v", relays)
	}
}

func TestGitOutboxPrivateDiscoveryNeverUsesUnconfiguredRelay(t *testing.T) {
	if gitOutboxAllowedRelay("wss://other.example", true, []string{"https://peer.example/git"}) {
		t.Fatal("unconfigured private relay was allowed")
	}
	if !gitOutboxAllowedRelay("wss://peer.example/git", true, []string{"https://peer.example/git"}) {
		t.Fatal("configured private relay was rejected")
	}
}

func TestGitOutboxRelayTagsUseWriteCapableNIP65Entries(t *testing.T) {
	list := event.Event{Kind: 10002, Tags: [][]string{{"r", "wss://read.example", "read"}, {"r", "wss://write.example", "write"}, {"r", "wss://both.example"}}}
	got := gitOutboxRelayTags(list)
	if len(got) != 2 || got[0] != "wss://write.example" || got[1] != "wss://both.example" {
		t.Fatalf("relays = %#v", got)
	}
}

func TestGitOutboxRelayTagsUseGroupEntriesForKind10317(t *testing.T) {
	list := event.Event{Kind: 10317, Tags: [][]string{{"r", "wss://wrong.example"}, {"g", "wss://group.example"}}}
	got := gitOutboxRelayTags(list)
	if len(got) != 1 || got[0] != "wss://group.example" {
		t.Fatalf("relays = %#v", got)
	}
}

func TestGitConversationAuthorsAreLimitedToAdmittedParticipants(t *testing.T) {
	owner := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	maintainer := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	participant := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	authors := gitConversationAuthors([]event.Event{{Kind: event.KIND_GIT_ISSUE, PubKey: participant}, {Kind: event.KIND_PROFILE, PubKey: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}}, owner, []string{maintainer})
	if len(authors) != 3 || authors[0] != owner || authors[1] != maintainer || authors[2] != participant {
		t.Fatalf("authors = %#v", authors)
	}
}

func TestGitConversationAuthorsDoesNotDropParticipantsBeforeBatching(t *testing.T) {
	events := make([]event.Event, 40)
	for i := range events {
		events[i] = event.Event{Kind: event.KIND_GIT_ISSUE, PubKey: fmt.Sprintf("%064x", i+1)}
	}
	if got := gitConversationAuthors(events, "", nil); len(got) != len(events) {
		t.Fatalf("authors = %d, want %d", len(got), len(events))
	}
}

func TestGitOutboxAuthorBatchRotatesPastLimit(t *testing.T) {
	events := make([]event.Event, 40)
	for i := range events {
		events[i] = event.Event{Kind: event.KIND_GIT_ISSUE, PubKey: fmt.Sprintf("%064x", i+1)}
	}
	authors := gitConversationAuthors(events, "", nil)
	first := boundedGitAuthorsFrom(authors, 0)
	second := boundedGitAuthorsFrom(authors, gitOutboxAuthorLimit)
	if len(first) != gitOutboxAuthorLimit || len(second) != len(authors)-gitOutboxAuthorLimit {
		t.Fatalf("batch sizes = %d and %d", len(first), len(second))
	}
	if first[0] == second[0] {
		t.Fatal("rotated batch repeated its first author")
	}
}

func TestRotateGitRelaysChangesStartingPoint(t *testing.T) {
	input := []string{"wss://one.example", "wss://two.example", "wss://three.example"}
	got := rotateGitRelays(input, 1)
	if len(got) != 3 || got[0] != input[1] || got[2] != input[0] {
		t.Fatalf("rotated relays = %#v", got)
	}
	if input[0] != "wss://one.example" {
		t.Fatal("rotation modified input")
	}
}

func TestDiscoverGitOutboxesRequiresTransport(t *testing.T) {
	_, err := discoverGitOutboxes(context.Background(), gitOutboxOptions{}, []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	if err == nil || err.Error() != "GRASP-03: outbox transport is required" {
		t.Fatalf("error = %v", err)
	}
}

func TestDiscoverGitOutboxesIsolatesUnavailableSeeds(t *testing.T) {
	secret, err := event.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	item := event.Event{Kind: 10002, CreatedAt: 1, Tags: [][]string{{"r", "wss://outbox.example"}}}
	if err := event.Sign(&item, secret); err != nil {
		t.Fatal(err)
	}
	if err := event.Validate(item); err != nil {
		t.Fatal(err)
	}
	if got := gitOutboxRelayTags(item); len(got) != 1 {
		t.Fatalf("item relays = %#v", got)
	}
	transport := gitOutboxTransport{
		queries: map[string][]event.Event{"wss://healthy.example": {item}},
		errors:  map[string]error{"wss://down.example": fmt.Errorf("connection refused")},
	}
	authors := []string{item.PubKey}
	if got := boundedGitRelays([]string{"wss://down.example", "wss://healthy.example"}, false, nil); len(got) != 2 {
		t.Fatalf("seed relays = %#v", got)
	}
	got, err := discoverGitOutboxes(context.Background(), gitOutboxOptions{Transport: transport, Seeds: []string{"wss://down.example", "wss://healthy.example"}}, authors)
	if err == nil {
		t.Fatal("expected isolated seed error to be reported")
	}
	if len(got) != 1 || got[0] != "wss://outbox.example" {
		t.Fatalf("relays = %#v, err = %v", got, err)
	}
}

func containsGitKind(kinds []int, wanted int) bool {
	for _, kind := range kinds {
		if kind == wanted {
			return true
		}
	}
	return false
}

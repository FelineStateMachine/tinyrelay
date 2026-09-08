package daemon

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
)

const (
	gitOutboxAuthorLimit = 32
	gitOutboxRelayLimit  = 24
)

// gitOutboxOptions describes the bounded, read-only part of GRASP-03. Seeds
// are the repository's announced relays. Private repositories must also set
// PrivatePeers; discovered public relays are never used for private work.
type gitOutboxOptions struct {
	Transport    replication.PullTransport
	Seeds        []string
	Private      bool
	PrivatePeers []string
	AuthorOffset int
	SeedOffset   int
}

// discoverGitOutboxes asks announced relays for the NIP-65 lists of authors
// who took part in a repository conversation. A failed author or seed is
// isolated so one unavailable relay cannot prevent the remaining discussion
// from being imported.
func discoverGitOutboxes(ctx context.Context, options gitOutboxOptions, authors []string) ([]string, error) {
	items, err := discoverGitOutboxEvents(ctx, options, authors)
	result := make([]string, 0, gitOutboxRelayLimit)
	seen := make(map[string]struct{}, gitOutboxRelayLimit)
	for _, item := range items {
		for _, relay := range gitOutboxRelayTags(item) {
			if !gitOutboxAllowedRelay(relay, options.Private, options.PrivatePeers) {
				continue
			}
			canonical := normalizeRelayGit(relay)
			if _, ok := seen[canonical]; ok {
				continue
			}
			seen[canonical] = struct{}{}
			result = append(result, canonical)
			if len(result) == gitOutboxRelayLimit {
				return result, err
			}
		}
	}
	return result, err
}

// discoverGitOutboxEvents returns the newest valid authored list of each
// supported kind. Callers should ingest these events with OriginImport so the
// relay list becomes available after a restart and to normal replication.
func discoverGitOutboxEvents(ctx context.Context, options gitOutboxOptions, authors []string) ([]event.Event, error) {
	if options.Transport == nil {
		return nil, errors.New("GRASP-03: outbox transport is required")
	}
	authors = boundedGitAuthorsFrom(authors, options.AuthorOffset)
	seeds := boundedGitRelaysFrom(options.Seeds, options.Private, options.PrivatePeers, options.SeedOffset)
	if len(authors) == 0 || len(seeds) == 0 {
		return nil, nil
	}
	newest := make(map[string]event.Event, len(authors)*3)
	var queryErrors []error
	workCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stop := false
	for _, seed := range seeds {
		for _, author := range authors {
			for _, kind := range []int{event.KIND_PROFILE, 10002, 10317} {
				if err := workCtx.Err(); err != nil {
					queryErrors = append(queryErrors, err)
					stop = true
					break
				}
				items, err := options.Transport.Query(workCtx, seed, gitOutboxFilter(author, kind))
				if err != nil {
					if len(queryErrors) < 8 {
						queryErrors = append(queryErrors, err)
					}
					continue
				}
				for _, item := range items {
					if event.Validate(item) != nil || item.PubKey != author || item.Kind != kind {
						continue
					}
					key := author + ":" + strconv.Itoa(item.Kind)
					if current, ok := newest[key]; !ok || item.CreatedAt > current.CreatedAt || (item.CreatedAt == current.CreatedAt && item.ID < current.ID) {
						newest[key] = item
					}
				}
			}
			if stop {
				break
			}
		}
		if stop {
			break
		}
	}
	result := make([]event.Event, 0, len(newest))
	for _, item := range newest {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].PubKey == result[j].PubKey {
			return result[i].Kind < result[j].Kind
		}
		return result[i].PubKey < result[j].PubKey
	})
	return result, errors.Join(queryErrors...)
}

func gitOutboxFilter(author string, kind int) event.Filter {
	limit := 1
	return event.Filter{Authors: []string{author}, Kinds: []int{kind}, Tags: map[string][]string{}, Limit: &limit}
}

// gitConversationAuthors returns the repository participants whose outboxes
// may contain replies. The caller supplies events already admitted to the
// repository conversation, so arbitrary relay authors cannot expand the
// discovery set.
func gitConversationAuthors(events []event.Event, owner string, maintainers []string) []string {
	authors := make([]string, 0, len(maintainers)+1+len(events))
	appendAuthor := func(author string) {
		if len(author) != 64 || strings.Trim(author, "0123456789abcdefABCDEF") != "" {
			return
		}
		for _, existing := range authors {
			if existing == author {
				return
			}
		}
		authors = append(authors, author)
	}
	appendAuthor(owner)
	for _, maintainer := range maintainers {
		appendAuthor(maintainer)
	}
	for _, item := range events {
		switch item.Kind {
		case event.KIND_GIT_PATCH, event.KIND_GIT_PR, event.KIND_GIT_PR_UPDATE, event.KIND_GIT_ISSUE, 1111, 1630, 1631, 1632, 1633:
			appendAuthor(item.PubKey)
		}
	}
	return authors
}

func boundedGitAuthors(authors []string) []string {
	return boundedGitAuthorsFrom(authors, 0)
}

func boundedGitAuthorsFrom(authors []string, offset int) []string {
	seen := make(map[string]struct{}, len(authors))
	result := make([]string, 0, minGitInt(len(authors), gitOutboxAuthorLimit))
	if offset < 0 {
		offset = 0
	}
	valid := 0
	for _, author := range authors {
		if len(author) != 64 || strings.Trim(author, "0123456789abcdefABCDEF") != "" {
			continue
		}
		if _, ok := seen[author]; ok {
			continue
		}
		seen[author] = struct{}{}
		if valid < offset {
			valid++
			continue
		}
		valid++
		result = append(result, author)
		if len(result) == gitOutboxAuthorLimit {
			break
		}
	}
	return result
}

func boundedGitRelays(relays []string, private bool, peers []string) []string {
	return boundedGitRelaysFrom(relays, private, peers, 0)
}

func boundedGitRelaysFrom(relays []string, private bool, peers []string, offset int) []string {
	seen := make(map[string]struct{}, len(relays))
	valid := make([]string, 0, len(relays))
	for _, relay := range relays {
		if !gitOutboxAllowedRelay(relay, private, peers) {
			continue
		}
		canonical := normalizeRelayGit(relay)
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		valid = append(valid, canonical)
	}
	if len(valid) == 0 {
		return nil
	}
	start := offset % len(valid)
	if start < 0 {
		start = 0
	}
	rotated := append(append([]string(nil), valid[start:]...), valid[:start]...)
	result := rotated
	if len(result) > gitOutboxRelayLimit {
		result = result[:gitOutboxRelayLimit]
	}
	return result
}

func gitOutboxRelayTags(item event.Event) []string {
	name := ""
	switch item.Kind {
	case 10002:
		name = "r"
	case 10317:
		name = "g"
	default:
		return nil
	}
	result := make([]string, 0)
	for _, tag := range item.Tags {
		if len(tag) < 2 || tag[0] != name {
			continue
		}
		if item.Kind == 10002 && len(tag) > 2 && strings.EqualFold(tag[2], "read") {
			continue
		}
		result = append(result, tag[1])
	}
	return result
}

func gitOutboxAllowedRelay(raw string, private bool, peers []string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Fragment != "" || u.Host == "" {
		return false
	}
	if u.Scheme == "http" {
		u.Scheme = "ws"
	} else if u.Scheme == "https" {
		u.Scheme = "wss"
	}
	if (u.Scheme != "ws" && u.Scheme != "wss") || !safeGitOutboxHost(u.Hostname()) {
		return false
	}
	if !private {
		return true
	}
	return privatePeerMatch(u.String(), peers)
}

func safeGitOutboxHost(host string) bool {
	return host != "" && !strings.ContainsAny(host, " \t\r\n")
}

func minGitInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

package daemon

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// gitSync is the GRASP-02 callback passed to GitRelay. It repairs every
// signed repository tip from the announcement's clone/relay sources before
// allowing the Git relay to expose the state.
func (t *Tenant) gitSync(ctx context.Context, repo gitrelay.Repository) error {
	if len(repo.Refs) == 0 {
		return nil
	}
	if len(gitrelay.RepairSources(repo)) == 0 {
		return errors.New("GRASP-02: repository has no repair source")
	}
	t.git.ConfigurePrivatePeers(t.Policy().PrivatePeers, t.privateHTTPAuth)
	return t.git.FetchMissing(ctx, repo, gitrelay.RepairSources(repo), repo.Refs)
}

// gitEventSync is the GRASP-03 callback. It imports NIP-34 repository
// metadata from announced relays through the hardened replication transport;
// ordinary event policy and GitRelay admission remain the final gates.
func (t *Tenant) gitEventSync(ctx context.Context, repo gitrelay.Repository) error {
	if len(repo.Relays) == 0 {
		return nil
	}
	filters := gitEventFilters(repo)
	for _, target := range repo.Relays {
		if sameRelayGit(target, t.RelayURL(), t.publicURL) {
			continue
		}
		transport := &replication.NostrTransport{Dialer: t.gitEventDialer(ctx, target)}
		seen := make(map[string]struct{})
		roots := make([]string, 0, 16)
		for _, filter := range filters {
			items, err := transport.Query(ctx, target, filter)
			if err != nil {
				return err
			}
			sort.SliceStable(items, func(i, j int) bool {
				return metadataEventOrder(items[i].Kind) < metadataEventOrder(items[j].Kind)
			})
			for _, item := range items {
				if _, ok := seen[item.ID]; ok {
					continue
				}
				seen[item.ID] = struct{}{}
				if item.Kind == event.KIND_GIT_PATCH || item.Kind == event.KIND_GIT_PR || item.Kind == event.KIND_GIT_ISSUE {
					roots = append(roots, item.ID)
				}
				if err := t.ingest(ctx, item, replication.OriginImport); err != nil && !errors.Is(err, storage.ErrDuplicate) && !errors.Is(err, storage.ErrReplaced) {
					return err
				}
			}
		}
		for _, key := range []string{"e", "E"} {
			if len(roots) == 0 {
				break
			}
			items, err := transport.Query(ctx, target, event.Filter{Kinds: []int{event.KIND_GIT_PR_UPDATE, 1111, 1630, 1631, 1632, 1633}, Tags: map[string][]string{key: roots}})
			if err != nil {
				return err
			}
			for _, item := range items {
				if _, ok := seen[item.ID]; ok {
					continue
				}
				seen[item.ID] = struct{}{}
				if err := t.ingest(ctx, item, replication.OriginImport); err != nil && !errors.Is(err, storage.ErrDuplicate) && !errors.Is(err, storage.ErrReplaced) {
					return err
				}
			}
		}
	}
	return nil
}

func metadataEventOrder(kind int) int {
	if kind == event.KIND_REPO {
		return 0
	}
	return 1
}

func (t *Tenant) gitEventDialer(ctx context.Context, target string) replication.WebsocketDialer {
	dialer := replication.WebsocketDialer{AllowPrivate: t.app.cfg.AllowPrivateRelays, MaxMessageBytes: t.app.cfg.MaxMessageBytes}
	if !t.privatePeerReady(ctx, target) {
		return dialer
	}
	dialer.Authenticate = func(authCtx context.Context, relayURL, challenge string) (event.Event, error) {
		if !t.PrivateServiceEnabled() || !privatePeerMatch(relayURL, t.Policy().PrivatePeers) {
			return event.Event{}, errors.New("private peer: authentication is no longer authorized")
		}
		return t.records.SignNIP42(authCtx, relayURL, challenge, 0)
	}
	return dialer
}

func gitEventFilters(repo gitrelay.Repository) []event.Filter {
	authors := append([]string{repo.Owner}, repo.Maintainers...)
	metadataKinds := []int{event.KIND_REPO, event.KIND_REPO_STATE}
	collaborationKinds := []int{event.KIND_GIT_PATCH, event.KIND_GIT_PR, event.KIND_GIT_PR_UPDATE, event.KIND_GIT_ISSUE, 1111, 1630, 1631, 1632, 1633}
	coordinate := "30617:" + repo.Owner + ":" + repo.Identifier
	return []event.Filter{
		{Kinds: metadataKinds, Authors: authors, Tags: map[string][]string{"d": {repo.Identifier}}},
		{Kinds: collaborationKinds, Tags: map[string][]string{"a": {coordinate}}},
		{Kinds: collaborationKinds, Tags: map[string][]string{"A": {coordinate}}},
	}
}

func sameRelayGit(a, b, c string) bool {
	a, b, c = normalizeRelayGit(a), normalizeRelayGit(b), normalizeRelayGit(c)
	return a != "" && (a == b || a == c)
}

func normalizeRelayGit(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	return strings.TrimRight(u.String(), "/")
}

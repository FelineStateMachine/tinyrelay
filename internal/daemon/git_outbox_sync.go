package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

const gitOutboxOffsetPrefix = "grasp03.outbox-offset."

type tenantGitOutboxTransport struct {
	tenant *Tenant
	repo   gitrelay.Repository
}

func (t tenantGitOutboxTransport) Query(ctx context.Context, target string, filter event.Filter) ([]event.Event, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	transport, err := t.tenant.gitTargetTransport(requestCtx, t.repo, target)
	if err != nil {
		return nil, err
	}
	return transport.Query(requestCtx, target, filter)
}

// gitOutboxSync imports participant relay lists, then asks each discovered
// outbox for the repository conversation. List events are ingested first so
// the discovered routing remains durable after a restart.
func (t *Tenant) gitOutboxSync(ctx context.Context, repo gitrelay.Repository) error {
	if t == nil || t.store == nil {
		return errors.New("GRASP-03: tenant store is required")
	}
	events, err := t.gitConversationEvents(ctx, repo)
	if err != nil {
		return err
	}
	authors := gitConversationAuthors(events, repo.Owner, repo.Maintainers)
	offset := t.gitOutboxOffset(ctx, repo)
	selectedAuthors := boundedGitAuthorsFrom(authors, offset%maxGitInt(len(authors), 1))
	private := repo.Private || t.PrivateServiceEnabled()
	options := gitOutboxOptions{Transport: tenantGitOutboxTransport{tenant: t, repo: repo}, Seeds: repo.Relays, Private: private, PrivatePeers: t.Policy().PrivatePeers, AuthorOffset: 0, SeedOffset: offset}
	listEvents, err := t.gitLocalOutboxEvents(ctx, selectedAuthors)
	if err != nil {
		return err
	}
	discovered, discoverErr := discoverGitOutboxEvents(ctx, options, selectedAuthors)
	listEvents = mergeGitOutboxEvents(listEvents, discovered)
	var syncErrors []error
	if err := t.ingestGitOutboxEvents(ctx, discovered); err != nil {
		syncErrors = append(syncErrors, err)
	}
	if discoverErr != nil {
		syncErrors = append(syncErrors, discoverErr)
	}
	if len(authors) > 0 {
		if err := t.store.PutSetting(ctx, gitOutboxOffsetKey(repo), (offset+gitOutboxAuthorLimit)%len(authors)); err != nil {
			syncErrors = append(syncErrors, fmt.Errorf("GRASP-03: save outbox offset: %w", err))
		}
	}
	targets := gitOutboxRelays(listEvents, options.Private, options.PrivatePeers)
	targets = rotateGitRelays(targets, offset)
	for _, target := range targets {
		targetCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		if err := t.gitSyncPeer(targetCtx, repo, target); err != nil {
			syncErrors = append(syncErrors, fmt.Errorf("GRASP-03: sync %s: %w", target, err))
		}
		cancel()
	}
	return errors.Join(syncErrors...)
}

func (t *Tenant) gitLocalOutboxEvents(ctx context.Context, authors []string) ([]event.Event, error) {
	result := make([]event.Event, 0, len(authors)*3)
	for _, author := range authors {
		for _, kind := range []int{event.KIND_PROFILE, 10002, 10317} {
			rows, err := t.store.Query(ctx, event.Filter{Authors: []string{author}, Kinds: []int{kind}, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
			if err != nil {
				return nil, fmt.Errorf("GRASP-03: read cached outbox: %w", err)
			}
			if len(rows.Events) > 0 {
				result = append(result, rows.Events[0])
			}
		}
	}
	return result, nil
}

func rotateGitRelays(relays []string, offset int) []string {
	if len(relays) == 0 {
		return nil
	}
	start := offset % len(relays)
	return append(append([]string(nil), relays[start:]...), relays[:start]...)
}

func maxGitInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func mergeGitOutboxEvents(existing, incoming []event.Event) []event.Event {
	result := append([]event.Event(nil), existing...)
	for _, item := range incoming {
		found := false
		for i := range result {
			if result[i].PubKey != item.PubKey || result[i].Kind != item.Kind {
				continue
			}
			found = true
			if item.CreatedAt > result[i].CreatedAt || (item.CreatedAt == result[i].CreatedAt && item.ID < result[i].ID) {
				result[i] = item
			}
			break
		}
		if !found {
			result = append(result, item)
		}
	}
	return result
}

func gitOutboxRelays(items []event.Event, private bool, peers []string) []string {
	result := make([]string, 0, gitOutboxRelayLimit)
	seen := make(map[string]struct{}, gitOutboxRelayLimit)
	for _, item := range items {
		for _, raw := range gitOutboxRelayTags(item) {
			if !gitOutboxAllowedRelay(raw, private, peers) {
				continue
			}
			canonical := normalizeRelayGit(raw)
			if _, ok := seen[canonical]; ok {
				continue
			}
			seen[canonical] = struct{}{}
			result = append(result, canonical)
			if len(result) == gitOutboxRelayLimit {
				return result
			}
		}
	}
	return result
}

func (t *Tenant) ingestGitOutboxEvents(ctx context.Context, items []event.Event) error {
	for _, item := range items {
		if err := t.ingest(ctx, item, replication.OriginImport); err != nil && !errors.Is(err, storage.ErrDuplicate) && !errors.Is(err, storage.ErrReplaced) {
			return fmt.Errorf("GRASP-03: import outbox list: %w", err)
		}
	}
	return nil
}

func (t *Tenant) gitOutboxOffset(ctx context.Context, repo gitrelay.Repository) int {
	var offset int
	if err := t.store.GetSetting(ctx, gitOutboxOffsetKey(repo), &offset); err != nil || offset < 0 {
		return 0
	}
	return offset
}

func gitOutboxOffsetKey(repo gitrelay.Repository) string {
	return gitOutboxOffsetPrefix + repo.Owner + ":" + repo.Identifier
}

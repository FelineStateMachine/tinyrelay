package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

const maxGitLivePeers = 32

// maxGitLiveRoots bounds the conversation roots one live subscription follows.
const maxGitLiveRoots = 100

type gitLiveWorker struct {
	cancel  context.CancelFunc
	done    chan struct{}
	started time.Time
}
type gitLiveTarget struct {
	repo   gitrelay.Repository
	target string
}

// runGitLive reconciles the current repository snapshot with live workers. A
// worker is removed as soon as its repository, relay, feature or private-peer
// authorization disappears. Rotation gives relays beyond the socket budget a
// turn on every reconciliation pass.
func (t *Tenant) runGitLive(ctx context.Context) {
	workers := make(map[string]gitLiveWorker)
	var cursor int
	defer stopGitLiveWorkers(workers)
	for {
		if !t.gitLiveEnabled() {
			stopGitLiveWorkers(workers)
			workers = make(map[string]gitLiveWorker)
		} else {
			cursor = t.reconcileGitLive(ctx, workers, cursor)
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (t *Tenant) gitLiveEnabled() bool {
	p := t.Policy()
	return p.Features.Grasp && p.Features.Grasp02 && t.git != nil
}

func (t *Tenant) reconcileGitLive(ctx context.Context, workers map[string]gitLiveWorker, cursor int) int {
	desired := t.gitLiveTargets(ctx)
	keys := make([]string, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		stopGitLiveWorkers(workers)
		return 0
	}
	if cursor >= len(keys) {
		cursor = 0
	}
	allowed := gitLiveCohort(keys, cursor)
	now := time.Now()
	for key, worker := range workers {
		_, wanted := desired[key]
		_, inCohort := allowed[key]
		if !wanted || (!inCohort && now.Sub(worker.started) >= gitLiveDwell) {
			worker.cancel()
			if worker.done != nil {
				<-worker.done
			}
			delete(workers, key)
		}
	}
	allowedKeys := make([]string, 0, len(allowed))
	for key := range allowed {
		allowedKeys = append(allowedKeys, key)
	}
	sort.Strings(allowedKeys)
	for _, key := range allowedKeys {
		if len(workers) >= maxGitLivePeers {
			break
		}
		if _, ok := workers[key]; ok {
			continue
		}
		workerCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		workers[key] = gitLiveWorker{cancel: cancel, done: done, started: now}
		go func(repo gitrelay.Repository, target string) {
			defer close(done)
			t.gitLivePeer(workerCtx, repo, target)
		}(desired[key].repo, desired[key].target)
	}
	if len(keys) > maxGitLivePeers {
		return (cursor + maxGitLivePeers) % len(keys)
	}
	return cursor
}

func gitLiveCohort(keys []string, cursor int) map[string]struct{} {
	allowed := make(map[string]struct{}, maxGitLivePeers)
	if len(keys) == 0 {
		return allowed
	}
	cursor %= len(keys)
	if cursor < 0 {
		cursor += len(keys)
	}
	for i := 0; i < len(keys) && i < maxGitLivePeers; i++ {
		allowed[keys[(cursor+i)%len(keys)]] = struct{}{}
	}
	return allowed
}

const gitLiveDwell = 2 * time.Minute

func (t *Tenant) gitLiveTargets(ctx context.Context) map[string]gitLiveTarget {
	result := make(map[string]gitLiveTarget)
	for _, repo := range t.git.Repositories() {
		targets := append([]string(nil), repo.Relays...)
		if t.Policy().Features.Grasp03 {
			if authors, err := t.gitLiveConversationAuthors(ctx, repo); err == nil {
				offset := t.gitOutboxOffset(ctx, repo)
				authors = rotateGitAuthors(authors, offset)
				if lists, err := t.gitLocalOutboxEvents(ctx, authors); err == nil {
					targets = append(targets, gitOutboxRelays(lists, repo.Private, t.Policy().PrivatePeers)...)
				}
			}
		}
		for _, target := range targets {
			target = normalizeRelayGit(target)
			if target == "" || sameRelayGit(target, t.RelayURL(), t.publicURL) || !t.gitLiveAuthorized(repo, target) {
				continue
			}
			key := strings.Join([]string{repo.Owner, repo.Identifier, target, repo.EventID, fmt.Sprint(repo.Private)}, "\x00")
			result[key] = gitLiveTarget{repo: repo, target: target}
		}
	}
	return result
}

func (t *Tenant) gitLiveConversationAuthors(ctx context.Context, repo gitrelay.Repository) ([]string, error) {
	items := make([]event.Event, 0, 256)
	for _, filter := range gitEventFilters(repo)[1:] {
		rows, err := t.store.Query(ctx, filter, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 256})
		if err != nil {
			return nil, err
		}
		items = append(items, rows.Events...)
	}
	return gitConversationAuthors(items, repo.Owner, repo.Maintainers), nil
}

func rotateGitAuthors(authors []string, offset int) []string {
	if len(authors) == 0 {
		return nil
	}
	offset %= len(authors)
	if offset < 0 {
		offset += len(authors)
	}
	rotated := append([]string(nil), authors[offset:]...)
	return append(rotated, authors[:offset]...)
}

func (t *Tenant) gitLiveAuthorized(repo gitrelay.Repository, target string) bool {
	if !repo.Private && !privatePolicy(t.Policy()) {
		return true
	}
	return privatePeerMatch(target, t.Policy().PrivatePeers)
}

func stopGitLiveWorkers(workers map[string]gitLiveWorker) {
	for key, worker := range workers {
		worker.cancel()
		if worker.done != nil {
			<-worker.done
		}
		delete(workers, key)
	}
}

// gitLivePeer performs bounded history overlap before opening subscriptions.
// One subscription follows all repository and conversation filters.
func (t *Tenant) gitLivePeer(ctx context.Context, repo gitrelay.Repository, target string) {
	backoff := time.Second
	for ctx.Err() == nil {
		historyCtx, historyCancel := context.WithTimeout(ctx, 20*time.Second)
		if err := t.gitSyncPeer(historyCtx, repo, target); err != nil && ctx.Err() == nil {
			t.app.telemetry.Logger().Debug("Git live history overlap incomplete", "target", target, "error", err)
		}
		historyCancel()
		conversation, err := t.gitConversationEvents(ctx, repo)
		if err != nil {
			conversation = nil
		}
		filters := gitLiveFiltersForRoots(repo, conversation)
		readyCtx, readyCancel := context.WithTimeout(ctx, 5*time.Second)
		transport, err := t.gitTargetTransport(readyCtx, repo, target)
		readyCancel()
		if err != nil {
			backoff = nextGitLiveBackoff(backoff)
			if !waitGitLive(ctx, backoff) {
				return
			}
			continue
		}
		started := time.Now()
		subCtx, subCancel := context.WithTimeout(ctx, 2*time.Minute)
		err = transport.SubscribeFilters(subCtx, target, filters, func(item event.Event) error {
			t.ingestGitLive(ctx, item)
			return nil
		})
		subCancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if time.Since(started) >= 30*time.Second {
				backoff = time.Second
			}
			backoff = nextGitLiveBackoff(backoff)
			if !waitGitLive(ctx, backoff) {
				return
			}
		}
	}
}

func (t *Tenant) ingestGitLive(ctx context.Context, item event.Event) {
	err := t.ingest(ctx, item, replication.OriginImport)
	if errors.Is(err, storage.ErrDuplicate) || errors.Is(err, storage.ErrReplaced) {
		return
	}
}

func gitLiveFiltersForRoots(repo gitrelay.Repository, conversation []event.Event) []event.Filter {
	filters := gitEventFilters(repo)
	roots := make([]string, 0, len(conversation))
	for _, item := range conversation {
		if item.Kind == event.KIND_GIT_PATCH || item.Kind == event.KIND_GIT_PR || item.Kind == event.KIND_GIT_ISSUE {
			roots = append(roots, item.ID)
		}
	}
	if len(roots) == 0 {
		return filters
	}
	// One subscription carries every filter, so the request stays bounded to
	// the newest conversations. Older threads are covered by history passes.
	if len(roots) > maxGitLiveRoots {
		sorted := append([]event.Event(nil), conversation...)
		sortEventsNewestFirst(sorted)
		roots = roots[:0]
		for _, item := range sorted {
			if item.Kind == event.KIND_GIT_PATCH || item.Kind == event.KIND_GIT_PR || item.Kind == event.KIND_GIT_ISSUE {
				roots = append(roots, item.ID)
			}
			if len(roots) == maxGitLiveRoots {
				break
			}
		}
	}
	kinds := []int{event.KIND_GIT_PR_UPDATE, 1111, 1630, 1631, 1632, 1633}
	return append(filters,
		event.Filter{Kinds: kinds, Tags: map[string][]string{"e": roots}},
		event.Filter{Kinds: kinds, Tags: map[string][]string{"E": roots}},
	)
}

func nextGitLiveBackoff(current time.Duration) time.Duration {
	if current >= 5*time.Minute {
		return 5 * time.Minute
	}
	return current * 2
}

func waitGitLive(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func liveFilter(repo gitrelay.Repository) event.Filter {
	return gitEventFilters(repo)[1]
}

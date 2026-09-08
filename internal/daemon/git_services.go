package daemon

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/syncprotocol"
)

// gitSync repairs signed branch state. Conversation history is scheduled
// separately so a missing Git object cannot block issue or PR replication.
func (t *Tenant) gitSync(ctx context.Context, repo gitrelay.Repository) error {
	if len(repo.Refs) == 0 {
		return nil
	}
	repo.Private = repo.Private || t.PrivateServiceEnabled()
	t.git.ConfigurePrivatePeers(t.Policy().PrivatePeers, t.privateHTTPAuth)
	return t.git.FetchMissing(ctx, repo, gitrelay.RepairSources(repo), repo.Refs)
}

func (t *Tenant) gitEventSync(ctx context.Context, repo gitrelay.Repository) error {
	var result error
	targets := boundedGitRelays(repo.Relays, repo.Private || t.PrivateServiceEnabled(), t.Policy().PrivatePeers)
	setting := "grasp02.peer-offset." + repo.Owner + ":" + repo.Identifier
	var offset int
	_ = t.store.GetSetting(ctx, setting, &offset)
	if offset < 0 {
		offset = 0
	}
	targets = rotateGitRelays(targets, offset)
	historyCtx, historyCancel := context.WithTimeout(ctx, 20*time.Second)
	for index, target := range targets {
		if historyCtx.Err() != nil {
			break
		}
		if sameRelayGit(target, t.RelayURL(), t.publicURL) {
			continue
		}
		// Persist the next starting peer before a bounded network attempt.
		// An unavailable peer cannot monopolize every subsequent pass.
		result = errors.Join(result, t.store.PutSetting(ctx, setting, (offset+index+1)%len(targets)))
		peerCtx, cancel := context.WithTimeout(historyCtx, 8*time.Second)
		result = errors.Join(result, t.gitSyncPeer(peerCtx, repo, target))
		cancel()
	}
	historyCancel()
	repairCtx, repairCancel := context.WithTimeout(ctx, 5*time.Second)
	result = errors.Join(result, t.repairGitPullRequests(repairCtx, repo))
	repairCancel()
	if t.Policy().Features.Grasp03 {
		outboxCtx, outboxCancel := context.WithTimeout(ctx, 15*time.Second)
		result = errors.Join(result, t.gitOutboxSync(outboxCtx, repo))
		outboxCancel()
	}
	return withoutBudgetDeadline(ctx, result)
}

// withoutBudgetDeadline drops deadlines from the pass's own time budgets.
// Peer offsets and outbox offsets persist between passes, so running out of
// time leaves the repository incomplete rather than failed and keeps it on the
// regular cadence. Cancellation of the caller's context still propagates.
func withoutBudgetDeadline(parent context.Context, err error) error {
	if err == nil || parent.Err() != nil {
		return err
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var kept []error
		for _, item := range joined.Unwrap() {
			if item = withoutBudgetDeadline(parent, item); item != nil {
				kept = append(kept, item)
			}
		}
		return errors.Join(kept...)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

// gitTargetTransport checks the privacy boundary before sending any filters.
// An unverified private peer never falls back to an anonymous connection.
func (t *Tenant) gitTargetTransport(ctx context.Context, repo gitrelay.Repository, target string) (*replication.NostrTransport, error) {
	p := t.Policy()
	if !p.Features.Grasp || !p.Features.Grasp02 {
		return nil, errors.New("GRASP synchronization is disabled")
	}
	if !gitOutboxAllowedRelay(target, repo.Private || t.PrivateServiceEnabled(), p.PrivatePeers) {
		return nil, errors.New("GRASP synchronization target is not allowed")
	}
	if repo.Private || t.PrivateServiceEnabled() {
		if !t.PrivateServiceEnabled() || !privatePeerMatch(target, p.PrivatePeers) || !t.privatePeerReady(ctx, target) {
			return nil, errors.New("private peer is not ready")
		}
	}
	return &replication.NostrTransport{Dialer: t.gitEventDialer(repo.Private || t.PrivateServiceEnabled()), Timeout: 5 * time.Second}, nil
}

func (t *Tenant) gitSyncPeer(ctx context.Context, repo gitrelay.Repository, target string) error {
	transport, err := t.gitTargetTransport(ctx, repo, target)
	if err != nil {
		return err
	}
	var result error
	for _, filter := range gitEventFilters(repo) {
		result = errors.Join(result, t.gitPullFilter(ctx, transport, target, filter))
	}
	events, err := t.gitConversationEvents(ctx, repo)
	if err != nil {
		return errors.Join(result, err)
	}
	for _, filter := range gitReplyFilters(events) {
		result = errors.Join(result, t.gitPullFilter(ctx, transport, target, filter))
	}
	return result
}

func (t *Tenant) gitPullFilter(ctx context.Context, transport *replication.NostrTransport, target string, filter event.Filter) error {
	local, err := t.localSyncItems(ctx, filter)
	if err != nil {
		return err
	}
	items, pullErr := transport.QuerySynchronized(ctx, target, filter, local)
	sort.SliceStable(items, func(i, j int) bool { return metadataEventOrder(items[i].Kind) < metadataEventOrder(items[j].Kind) })
	for _, item := range items {
		if ctx.Err() != nil {
			return errors.Join(pullErr, ctx.Err())
		}
		if !event.Matches(filter, item) || event.Validate(item) != nil {
			continue
		}
		// Rejected remote events must not prevent later valid events from arriving.
		if err := t.ingest(ctx, item, replication.OriginImport); err != nil && !errors.Is(err, storage.ErrDuplicate) && !errors.Is(err, storage.ErrReplaced) {
			t.app.telemetry.Logger().Debug("Git sync event rejected", "kind", item.Kind, "error", err)
		}
	}
	return pullErr
}

func (t *Tenant) localSyncItems(ctx context.Context, filter event.Filter) ([]syncprotocol.Item, error) {
	rows, err := t.store.Query(ctx, filter, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 10001})
	if err != nil {
		return nil, err
	}
	if rows.More {
		return nil, replication.ErrPullIncomplete
	}
	items := make([]syncprotocol.Item, 0, len(rows.Events))
	for _, item := range rows.Events {
		items = append(items, syncprotocol.Item{ID: item.ID, Timestamp: item.CreatedAt})
	}
	return items, nil
}

func (t *Tenant) gitConversationEvents(ctx context.Context, repo gitrelay.Repository) ([]event.Event, error) {
	var items []event.Event
	for _, filter := range gitEventFilters(repo)[1:] {
		rows, err := t.store.Query(ctx, filter, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 10000})
		if err != nil {
			return nil, err
		}
		if rows.More {
			return nil, replication.ErrPullIncomplete
		}
		items = append(items, rows.Events...)
	}
	roots := uniqueEvents(items)
	for _, filter := range gitReplyFilters(roots) {
		rows, err := t.store.Query(ctx, filter, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 10000})
		if err != nil {
			return nil, err
		}
		if rows.More {
			return nil, replication.ErrPullIncomplete
		}
		items = append(items, rows.Events...)
	}
	return uniqueEvents(items), nil
}

func gitReplyFilters(items []event.Event) []event.Filter {
	var ids []string
	for _, item := range items {
		if item.Kind == 1617 || item.Kind == 1618 || item.Kind == 1621 {
			ids = append(ids, item.ID)
		}
	}
	var filters []event.Filter
	for start := 0; start < len(ids); start += 100 {
		end := start + 100
		if end > len(ids) {
			end = len(ids)
		}
		for _, key := range []string{"e", "E"} {
			filters = append(filters, event.Filter{Kinds: []int{1619, 1111, 1630, 1631, 1632, 1633}, Tags: map[string][]string{key: ids[start:end]}})
		}
	}
	return filters
}

func (t *Tenant) repairGitPullRequests(ctx context.Context, repo gitrelay.Repository) error {
	repo.Private = repo.Private || t.PrivateServiceEnabled()
	items, err := t.gitConversationEvents(ctx, repo)
	if err != nil {
		return err
	}
	t.git.ConfigurePrivatePeers(t.Policy().PrivatePeers, t.privateHTTPAuth)
	var result error
	for _, root := range items {
		if root.Kind != 1618 {
			continue
		}
		set := gitrelay.PullRequestRepair{Root: root}
		for _, update := range items {
			if update.Kind == 1619 && update.PubKey == root.PubKey && event.Tag(update, "E") == root.ID && event.Tag(update, "P") == root.PubKey {
				set.Updates = append(set.Updates, update)
			}
		}
		err := t.git.RepairPullRequestObjects(ctx, repo, set)
		result = errors.Join(result, err)
		if ctx.Err() != nil {
			return errors.Join(result, ctx.Err())
		}
	}
	return result
}

func metadataEventOrder(kind int) int {
	if kind == event.KIND_REPO {
		return 0
	}
	return 1
}

func (t *Tenant) gitEventDialer(private bool) replication.WebsocketDialer {
	dialer := replication.WebsocketDialer{AllowPrivate: t.app.cfg.AllowPrivateRelays, MaxMessageBytes: t.app.cfg.MaxMessageBytes}
	if !private {
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
	u.Host = strings.ToLower(u.Host)
	return strings.TrimRight(u.String(), "/")
}

func uniqueEvents(rows []event.Event) []event.Event {
	seen := make(map[string]struct{}, len(rows))
	result := make([]event.Event, 0, len(rows))
	for _, row := range rows {
		if _, ok := seen[row.ID]; ok {
			continue
		}
		seen[row.ID] = struct{}{}
		result = append(result, row)
	}
	return result
}

// sortEventsNewestFirst matches the stable storage order used by cursors.
func sortEventsNewestFirst(rows []event.Event) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt == rows[j].CreatedAt {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt > rows[j].CreatedAt
	})
}

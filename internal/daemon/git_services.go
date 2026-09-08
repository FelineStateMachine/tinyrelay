package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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
	if historyCtx.Err() != nil && ctx.Err() == nil {
		result = errors.Join(result, replication.ErrPullIncomplete)
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
	return gitSyncOutcome(ctx, result)
}

// gitSyncOutcome classifies a pass for the scheduler. Deadlines from the
// pass's own time budgets are dropped because peer and outbox offsets persist
// between passes. A pass whose only remaining condition is more history to
// fetch reports gitrelay.ErrIncomplete so it continues soon without counting
// as a failure. Cancellation of the caller's context still propagates.
func gitSyncOutcome(parent context.Context, err error) error {
	err = withoutBudgetDeadline(parent, err)
	if err == nil {
		return nil
	}
	if onlyPullIncomplete(err) {
		return gitrelay.ErrIncomplete
	}
	return err
}

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

func onlyPullIncomplete(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, item := range joined.Unwrap() {
			if !onlyPullIncomplete(item) {
				return false
			}
		}
		return true
	}
	return errors.Is(err, replication.ErrPullIncomplete)
}

// gitIngestRejected reports a deterministic admission decision. NIP-01
// prefixes mark policy rejections that repeat on every attempt; other errors
// are storage or lookup failures that may succeed later.
func gitIngestRejected(err error) bool {
	message := err.Error()
	for _, prefix := range []string{"blocked:", "invalid:", "restricted:", "auth-required:", "pow:"} {
		if strings.HasPrefix(message, prefix) {
			return true
		}
	}
	return false
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
	return &replication.NostrTransport{Dialer: t.gitEventDialer(repo.Private || t.PrivateServiceEnabled()), Timeout: 5 * time.Second, LegacyCache: t.gitLegacy}, nil
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
	if errors.Is(err, replication.ErrPullIncomplete) {
		return t.gitPullLargeFilter(ctx, transport, target, filter)
	}
	if err != nil {
		return err
	}
	items, pullErr := transport.QuerySynchronized(ctx, target, filter, local)
	return errors.Join(pullErr, t.ingestGitHistory(ctx, filter, items))
}

// A partial inventory is not safe for negentropy: the resulting missing-ID set
// can overflow its fetch cap. Refresh a small head first, then resume an older
// window. A slow historical query cannot prevent new events from being stored.
func (t *Tenant) gitPullLargeFilter(ctx context.Context, transport *replication.NostrTransport, target string, filter event.Filter) error {
	olderFilter, hasCursor, err := t.gitHistoryWindow(ctx, target, filter)
	if err != nil {
		return err
	}
	headCtx, cancelHead := gitQueryContext(ctx, 2*time.Second)
	head, headErr := queryGitHead(headCtx, transport, target, filter)
	cancelHead()
	headIngestErr := t.ingestGitHistory(ctx, filter, head)
	result := errors.Join(replication.ErrPullIncomplete, headErr, headIngestErr)
	key := gitHistoryCursorKey(target, filter)
	if !hasCursor {
		// A completed bounded head query seeds history; it never proves that
		// the remote inventory is exhausted. Failed queries cannot move a
		// timestamp cursor because events may have arrived out of order.
		if headErr == nil && headIngestErr == nil && len(head) > 0 {
			result = errors.Join(result, t.store.PutSetting(ctx, key, oldestGitHistoryCursor(head)))
		}
		return result
	}
	if ctx.Err() != nil {
		return errors.Join(result, ctx.Err())
	}
	olderCtx, cancelOlder := gitQueryContext(ctx, 5*time.Second)
	older, olderErr := transport.QueryPaginated(olderCtx, target, olderFilter)
	cancelOlder()
	olderIngestErr := t.ingestGitHistory(ctx, filter, older)
	result = errors.Join(result, olderErr, olderIngestErr)
	if olderIngestErr != nil {
		return result
	}
	if olderErr == nil {
		// Finished this sweep. The next pass starts another from the head,
		// allowing older events newly acquired by the peer to be discovered.
		result = errors.Join(result, t.store.PutSetting(ctx, key, storage.EventCursor{}))
	} else if errors.Is(olderErr, replication.ErrPullIncomplete) && len(older) > 0 {
		result = errors.Join(result, t.store.PutSetting(ctx, key, oldestGitHistoryCursor(older)))
	}
	return result
}

func (t *Tenant) ingestGitHistory(ctx context.Context, filter event.Filter, items []event.Event) error {
	sort.SliceStable(items, func(i, j int) bool { return metadataEventOrder(items[i].Kind) < metadataEventOrder(items[j].Kind) })
	var result error
	for _, item := range items {
		if ctx.Err() != nil {
			return errors.Join(result, ctx.Err())
		}
		if !event.Matches(filter, item) || event.Validate(item) != nil {
			continue
		}
		// A rejected event cannot prevent later valid events from arriving,
		// and a policy rejection repeats on every attempt, so it counts as
		// processed. A storage failure keeps the checkpoint so the window is
		// retried without skipping an event that was not persisted.
		if err := t.ingest(ctx, item, replication.OriginImport); err != nil && !errors.Is(err, storage.ErrDuplicate) && !errors.Is(err, storage.ErrReplaced) {
			t.app.telemetry.Logger().Debug("Git sync event rejected", "kind", item.Kind, "error", err)
			if !gitIngestRejected(err) {
				result = errors.Join(result, err)
			}
		}
	}
	return result
}

const gitHeadPageSize = 256

// Reserve time to ingest the response using the parent context. A query that
// expires may still have returned valid events worth retaining for the retry.
func gitQueryContext(ctx context.Context, maximum time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline) / 2; remaining < maximum {
			maximum = remaining
		}
	}
	return context.WithTimeout(ctx, maximum)
}

func queryGitHead(ctx context.Context, transport *replication.NostrTransport, target string, filter event.Filter) ([]event.Event, error) {
	limit := gitHeadPageSize
	filter.Limit = &limit
	return transport.Query(ctx, target, filter)
}

func oldestGitHistoryCursor(items []event.Event) storage.EventCursor {
	if len(items) == 0 {
		return storage.EventCursor{}
	}
	oldest := items[0]
	for _, item := range items[1:] {
		if item.CreatedAt < oldest.CreatedAt || (item.CreatedAt == oldest.CreatedAt && item.ID > oldest.ID) {
			oldest = item
		}
	}
	return storage.EventCursor{CreatedAt: oldest.CreatedAt, ID: oldest.ID}
}

func gitHistoryCursorKey(target string, filter event.Filter) string {
	raw, _ := json.Marshal(filter)
	sum := sha256.Sum256(raw)
	return "grasp02.history." + target + "." + hex.EncodeToString(sum[:])
}

func (t *Tenant) gitHistoryWindow(ctx context.Context, target string, filter event.Filter) (event.Filter, bool, error) {
	var cursor storage.EventCursor
	err := t.store.GetSetting(ctx, gitHistoryCursorKey(target, filter), &cursor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return filter, false, err
	}
	if cursor.CreatedAt > 0 {
		until := cursor.CreatedAt
		filter.Until = &until
		return filter, true, nil
	}
	return filter, false, nil
}

func (t *Tenant) localSyncItems(ctx context.Context, filter event.Filter) ([]syncprotocol.Item, error) {
	rows, err := t.store.Query(ctx, filter, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 10000})
	if err != nil {
		return nil, err
	}
	// A bounded inventory is intentionally reported as incomplete. The caller
	// switches to a bounded newest-first pull so a large local history cannot
	// overflow negentropy's missing-ID fetch cap.
	items := make([]syncprotocol.Item, 0, len(rows.Events))
	for _, item := range rows.Events {
		items = append(items, syncprotocol.Item{ID: item.ID, Timestamp: item.CreatedAt})
	}
	if rows.More {
		return items, replication.ErrPullIncomplete
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
		items = append(items, rows.Events...)
	}
	roots := uniqueEvents(items)
	for _, filter := range gitReplyFilters(roots) {
		rows, err := t.store.Query(ctx, filter, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 10000})
		if err != nil {
			return nil, err
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

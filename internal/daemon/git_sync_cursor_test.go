package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// cursorRelay is deliberately a small NIP-01 relay. It applies the filter's
// limit and until values, which makes the test exercise the same bounded head
// and paginated older queries used by production sync.
type cursorRelay struct {
	events       []event.Event
	stallHistory bool
}

type cursorSocket struct {
	relay     *cursorRelay
	responses [][]byte
}

type cursorDialer struct{ relay *cursorRelay }

func (d cursorDialer) Dial(context.Context, string) (replication.Socket, *http.Response, error) {
	return &cursorSocket{relay: d.relay}, nil, nil
}

func (s *cursorSocket) Read(ctx context.Context) ([]byte, error) {
	if len(s.responses) == 0 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	item := s.responses[0]
	s.responses = s.responses[1:]
	return item, nil
}

func (s *cursorSocket) Write(_ context.Context, data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || len(raw) < 3 {
		return nil
	}
	var kind, sub string
	if json.Unmarshal(raw[0], &kind) != nil || json.Unmarshal(raw[1], &sub) != nil || kind != "REQ" {
		return nil
	}
	f, err := event.ParseFilter(raw[2])
	if err != nil {
		return err
	}
	if f.Until != nil && s.relay.stallHistory {
		return nil
	}
	items := append([]event.Event(nil), s.relay.events...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt != items[j].CreatedAt {
			return items[i].CreatedAt > items[j].CreatedAt
		}
		return items[i].ID < items[j].ID
	})
	limit := 0
	if f.Limit != nil {
		limit = *f.Limit
	}
	for _, item := range items {
		if !event.Matches(f, item) {
			continue
		}
		if limit > 0 && len(s.responses) >= limit {
			break
		}
		message, _ := json.Marshal([]any{"EVENT", sub, item})
		s.responses = append(s.responses, message)
	}
	eose, _ := json.Marshal([]any{"EOSE", sub})
	s.responses = append(s.responses, eose)
	return nil
}

func (s *cursorSocket) Close(error) error { return nil }

func TestGitPullFilterAdvancesBeyondBoundedLocalInventory(t *testing.T) {
	_, tenant := testTenant(t)
	owner := tenant.Policy().Owner
	secret := "0000000000000000000000000000000000000000000000000000000000000001"
	now := time.Now().Unix()
	filter := event.Filter{Authors: []string{owner}, Kinds: []int{1}}
	for i := 0; i < 10001; i++ {
		// Seed the stored inventory directly; remote responses below still
		// undergo real signature validation and the normal ingestion path.
		item := event.Event{ID: fmt.Sprintf("%064x", i+1), PubKey: owner, Kind: 1, CreatedAt: now - int64(i+10000), Tags: [][]string{}, Content: "local"}
		if _, err := tenant.store.Save(context.Background(), item, storage.SaveOptions{Now: now, SearchMode: tenant.Policy().Features.Search}); err != nil {
			t.Fatal(err)
		}
	}
	remote := &cursorRelay{}
	for i := 0; i < gitHeadPageSize+1; i++ {
		item := event.Event{Kind: 1, CreatedAt: now - int64(i+1), Tags: [][]string{}, Content: "head"}
		if err := event.Sign(&item, secret); err != nil {
			t.Fatal(err)
		}
		remote.events = append(remote.events, item)
	}
	older := event.Event{Kind: 1, CreatedAt: now - 1000, Tags: [][]string{}, Content: "older"}
	if err := event.Sign(&older, secret); err != nil {
		t.Fatal(err)
	}
	remote.events = append(remote.events, older)
	transport := &replication.NostrTransport{Dialer: cursorDialer{relay: remote}, Timeout: time.Second}

	if err := tenant.gitPullFilter(context.Background(), transport, "ws://cursor.test", filter); !errors.Is(err, replication.ErrPullIncomplete) {
		t.Fatalf("first pull error = %v, want incomplete", err)
	}
	var cursor storage.EventCursor
	if err := tenant.store.GetSetting(context.Background(), gitHistoryCursorKey("ws://cursor.test", filter), &cursor); err != nil || cursor.CreatedAt == 0 {
		t.Fatalf("head cursor was not persisted: %v %#v", err, cursor)
	}
	if got := countContent(t, tenant, "older"); got != 0 {
		t.Fatalf("older event arrived during head seed: %d", got)
	}
	if err := tenant.gitPullFilter(context.Background(), transport, "ws://cursor.test", filter); !errors.Is(err, replication.ErrPullIncomplete) {
		t.Fatalf("second pull error = %v, want incomplete local inventory", err)
	}
	if got := countContent(t, tenant, "older"); got != 1 {
		t.Fatalf("older event count = %d, want 1", got)
	}

	// A completed sweep removes its cursor row so per-filter state cannot
	// accumulate; the next pass seeds a new head window.
	cursor = storage.EventCursor{}
	if err := tenant.store.GetSetting(context.Background(), gitHistoryCursorKey("ws://cursor.test", filter), &cursor); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("completed sweep left cursor %#v, err %v", cursor, err)
	}
	before := cursor
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tenant.gitPullFilter(ctx, transport, "ws://cursor.test", filter); err == nil {
		t.Fatal("canceled pull returned nil")
	}
	var after storage.EventCursor
	if err := tenant.store.GetSetting(context.Background(), gitHistoryCursorKey("ws://cursor.test", filter), &after); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("canceled pull advanced cursor from %#v to %#v", before, after)
	}
	// A stalled historical window must not consume the head or move its
	// checkpoint, even when the outer pass has a short deadline.
	before = storage.EventCursor{CreatedAt: older.CreatedAt, ID: older.ID}
	if err := tenant.store.PutSetting(context.Background(), gitHistoryCursorKey("ws://cursor.test", filter), before); err != nil {
		t.Fatal(err)
	}
	newest := event.Event{Kind: 1, CreatedAt: now, Tags: [][]string{}, Content: "live-head"}
	if err := event.Sign(&newest, secret); err != nil {
		t.Fatal(err)
	}
	remote.events = []event.Event{newest}
	remote.stallHistory = true
	transport.Timeout = time.Minute
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := tenant.gitPullFilter(ctx, transport, "ws://cursor.test", filter); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled history error = %v", err)
	}
	if got := countContent(t, tenant, "live-head"); got != 1 {
		t.Fatalf("stalled history blocked newest event: %d", got)
	}
	if err := tenant.store.GetSetting(context.Background(), gitHistoryCursorKey("ws://cursor.test", filter), &after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("stalled history moved cursor from %#v to %#v", before, after)
	}
}

func countContent(t *testing.T, tenant *Tenant, content string) int {
	t.Helper()
	rows, err := tenant.store.Query(context.Background(), event.Filter{Kinds: []int{1}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range rows.Events {
		if item.Content == content {
			count++
		}
	}
	return count
}

func TestGitSyncOutcomeClassifiesIncompleteAndBudgetDeadlines(t *testing.T) {
	ctx := context.Background()
	if got := gitSyncOutcome(ctx, errors.Join(context.DeadlineExceeded, replication.ErrPullIncomplete)); !errors.Is(got, gitrelay.ErrIncomplete) || errors.Is(got, replication.ErrPullIncomplete) {
		t.Fatalf("incomplete pass should map to gitrelay.ErrIncomplete, got %v", got)
	}
	if got := gitSyncOutcome(ctx, errors.Join(context.DeadlineExceeded, errors.New("dial failed"))); got == nil || errors.Is(got, gitrelay.ErrIncomplete) || errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("real errors must survive with deadlines dropped, got %v", got)
	}
	if got := gitSyncOutcome(ctx, context.DeadlineExceeded); got != nil {
		t.Fatalf("a bare budget deadline is not a failure, got %v", got)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got := gitSyncOutcome(canceled, context.DeadlineExceeded); got == nil {
		t.Fatal("a canceled caller context must propagate")
	}
	if !gitIngestRejected(errors.New("blocked: this pubkey is banned")) || gitIngestRejected(errors.New("check pubkey ban: disk I/O error")) {
		t.Fatal("rejection classification must follow NIP-01 prefixes")
	}
}

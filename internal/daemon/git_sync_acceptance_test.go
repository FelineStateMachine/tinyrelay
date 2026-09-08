package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/coder/websocket"
)

type gitSyncEventRelay struct {
	mu       sync.Mutex
	events   []event.Event
	live     *event.Event
	liveSent chan struct{}
	requests int
}

func (r *gitSyncEventRelay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Accept") == "application/nostr+json" {
		w.Header().Set("Content-Type", "application/nostr+json")
		_, _ = w.Write([]byte(`{"supported_grasps":["GRASP-01"],"private_service":false}`))
		return
	}
	conn, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")
	_, data, err := conn.Read(req.Context())
	if err != nil {
		return
	}
	var message []json.RawMessage
	if json.Unmarshal(data, &message) != nil || len(message) < 3 {
		return
	}
	var kind, subscription string
	_ = json.Unmarshal(message[0], &kind)
	_ = json.Unmarshal(message[1], &subscription)
	if kind != "REQ" {
		if kind == "NEG-OPEN" {
			response, _ := json.Marshal([]any{"NEG-ERR", subscription, "unsupported"})
			_ = conn.Write(req.Context(), websocket.MessageText, response)
		}
		return
	}
	filter, err := event.ParseFilter(message[2])
	if err != nil {
		return
	}
	r.mu.Lock()
	r.requests++
	items := append([]event.Event(nil), r.events...)
	r.mu.Unlock()
	for _, item := range items {
		if event.Matches(filter, item) {
			response, _ := json.Marshal([]any{"EVENT", subscription, item})
			_ = conn.Write(req.Context(), websocket.MessageText, response)
		}
	}
	response, _ := json.Marshal([]any{"EOSE", subscription})
	_ = conn.Write(req.Context(), websocket.MessageText, response)
	if strings.HasPrefix(subscription, "tiny-live-") && r.live != nil {
		live, _ := json.Marshal([]any{"EVENT", subscription, *r.live})
		_ = conn.Write(req.Context(), websocket.MessageText, live)
		if r.liveSent != nil {
			select {
			case r.liveSent <- struct{}{}:
			default:
			}
		}
	}
}

func (r *gitSyncEventRelay) requestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests
}

func TestGitSyncPeerImportsHistoricalIssueAndRootReplies(t *testing.T) {
	_, tenant := testTenant(t)
	tenant.app.cfg.AllowPrivateRelays = true
	p := tenant.Policy()
	p.Features.Grasp = true
	p.Features.Grasp02 = true
	p.Features.Grasp05 = true
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	owner := tenant.Policy().Owner
	secret := strings.Repeat("9", 63) + "8"
	author, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	relay := &gitSyncEventRelay{}
	server := httptest.NewServer(relay)
	t.Cleanup(server.Close)
	target := "ws" + strings.TrimPrefix(server.URL, "http")
	coordinate := "30617:" + owner + ":remote"
	root := signedCollaborationEvent(t, secret, event.KIND_GIT_ISSUE, 100,
		[][]string{{"a", coordinate}, {"subject", "Remote issue"}}, "historical")
	reply := signedCollaborationEvent(t, secret, 1111, 101,
		[][]string{{"E", root.ID}, {"K", "1621"}, {"P", author}}, "remote reply")
	relay.events = []event.Event{root, reply}
	repo := gitrelay.Repository{Owner: owner, Identifier: "remote", Relays: []string{target}}
	if err := tenant.gitSyncPeer(context.Background(), repo, target); err != nil {
		t.Fatal(err)
	}
	rows, err := tenant.store.Query(context.Background(), event.Filter{IDs: []string{root.ID, reply.ID}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Events) != 2 {
		t.Fatalf("synced collaboration events = %d, want 2", len(rows.Events))
	}
	if relay.requestCount() < 4 {
		t.Fatalf("multi-filter sync issued %d requests, want root and reply filters", relay.requestCount())
	}
}

func TestGitSyncPeerFeatureOffDoesNotDial(t *testing.T) {
	_, tenant := testTenant(t)
	relay := &gitSyncEventRelay{}
	server := httptest.NewServer(relay)
	t.Cleanup(server.Close)
	target := "ws" + strings.TrimPrefix(server.URL, "http")
	repo := gitrelay.Repository{Owner: tenant.Policy().Owner, Identifier: "remote", Relays: []string{target}}
	if err := tenant.gitSyncPeer(context.Background(), repo, target); err == nil {
		t.Fatal("feature-off sync unexpectedly succeeded")
	}
	if got := relay.requestCount(); got != 0 {
		t.Fatalf("feature-off sync dialed relay with %d requests", got)
	}
}

func TestGitLivePeerImportsEventAfterEOSE(t *testing.T) {
	_, tenant := testTenant(t)
	tenant.app.cfg.AllowPrivateRelays = true
	p := tenant.Policy()
	p.Features.Grasp = true
	p.Features.Grasp02 = true
	p.Features.Grasp05 = true
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	owner := tenant.Policy().Owner
	secret := strings.Repeat("a", 63) + "b"
	author, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	remote := &gitSyncEventRelay{liveSent: make(chan struct{}, 1)}
	server := httptest.NewServer(remote)
	t.Cleanup(server.Close)
	target := "ws" + strings.TrimPrefix(server.URL, "http")
	coordinate := "30617:" + owner + ":live"
	announcement := event.Event{Kind: event.KIND_REPO, PubKey: owner, CreatedAt: 199,
		Tags: [][]string{{"d", "live"}, {"clone", "http://relay.test/" + owner + "/live.git"}, {"relays", "ws://relay.test"}}}
	if err := event.Sign(&announcement, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.router.Publish(context.Background(), announcement, relay.Session{PubKeys: []string{owner}}); err != nil {
		t.Fatal(err)
	}
	root := signedCollaborationEvent(t, secret, event.KIND_GIT_ISSUE, 200,
		[][]string{{"a", coordinate}, {"subject", "Live issue"}}, "root")
	liveReply := signedCollaborationEvent(t, secret, 1111, 201,
		[][]string{{"E", root.ID}, {"K", "1621"}, {"P", author}}, "after EOSE")
	remote.events = []event.Event{root}
	remote.live = &liveReply
	repo := gitrelay.Repository{Owner: owner, Identifier: "live", Relays: []string{target}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		tenant.gitLivePeer(ctx, repo, target)
		close(done)
	}()
	select {
	case <-remote.liveSent:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("live subscription did not receive post-EOSE event")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rows, queryErr := tenant.store.Query(context.Background(), event.Filter{IDs: []string{liveReply.ID}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}})
		if queryErr == nil && len(rows.Events) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("live worker did not stop")
	}
	rows, err := tenant.store.Query(context.Background(), event.Filter{IDs: []string{liveReply.ID}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Events) != 1 {
		t.Fatalf("post-EOSE event was not imported: %d", len(rows.Events))
	}
}

func TestGitSyncPeerPrivatePeerReadinessRefusesBeforeWebsocket(t *testing.T) {
	_, tenant := testTenant(t)
	tenant.app.cfg.AllowPrivateRelays = true
	var websocketRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Accept") == "application/nostr+json" {
			w.Header().Set("Content-Type", "application/nostr+json")
			_, _ = w.Write([]byte(`{"supported_grasps":["GRASP-01"],"private_service":false}`))
			return
		}
		websocketRequests++
	}))
	t.Cleanup(server.Close)
	target := "ws" + strings.TrimPrefix(server.URL, "http")
	p := tenant.Policy()
	p.Features.Grasp = true
	p.Features.Grasp02 = true
	p.Features.Grasp08 = true
	p.Reads = "members"
	p.PrivatePeers = []string{target}
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	repo := gitrelay.Repository{Owner: p.Owner, Identifier: "private", Private: true, Relays: []string{target}}
	if err := tenant.gitSyncPeer(context.Background(), repo, target); err == nil {
		t.Fatal("unready private peer unexpectedly accepted")
	}
	if websocketRequests != 0 {
		t.Fatalf("private readiness failure opened %d websocket requests", websocketRequests)
	}
}

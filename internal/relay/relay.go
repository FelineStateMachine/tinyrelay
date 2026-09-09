// Package relay owns the websocket protocol boundary.  It deliberately keeps
// storage and policy behind Backend so the wire protocol cannot accidentally
// become the tenant's data model.
package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/syncprotocol"
	"github.com/coder/websocket"
)

type Session struct {
	PubKeys  []string
	RelayURL string
	RemoteIP string
}

// Backend publishes while the Router's write gate is held. Publish must make
// the event and any delivery intent durable before returning nil.
type Backend interface {
	Publish(context.Context, event.Event, Session) (string, error)
	Query(context.Context, []event.Filter, Session) ([]event.Event, error)
	Count(context.Context, []event.Filter, Session) (any, error)
}

// Reader-side privacy can be stricter than the query filter. It is checked for
// both historical and live delivery, including encrypted signer traffic.
type Reader interface {
	CanRead(event.Event, Session) bool
}

type FilterReader interface {
	CanReadFilter(event.Event, Session, *event.Filter) bool
}

type QueryHints interface {
	QueryHints(context.Context, []event.Filter, Session) ([]event.Event, []string, error)
}

// Policy changes can invalidate subscriptions. The Router removes them from
// clients immediately; the optional callback lets storage invalidate caches.
type PolicyChanges interface{ CloseSubscriptions(Session) }

// SyncBackend supplies the authorized event snapshot for a NIP-77 session.
// The Router holds its read gate while calling Sync, so a concurrent Publish
// cannot create an opening between snapshot selection and NEG-OPEN.
type SyncBackend interface {
	Sync(context.Context, event.Filter, Session) ([]syncprotocol.Item, error)
}

type Config struct {
	RelayURL        string
	RequestRelayURL func(*http.Request) string
	OnAuthenticate  func(context.Context, Session) error
	MaxMessageBytes int64
	MaxPendingBytes int
	ReadLimit       time.Duration
	WriteLimit      time.Duration
	PingInterval    time.Duration
	OriginPatterns  []string
	OnConnection    func(int)
	OnSubscription  func(int)
}

func (c Config) withDefaults() Config {
	if c.MaxMessageBytes < 0 {
		c.MaxMessageBytes = 512 << 10
	}
	if c.MaxPendingBytes <= 0 {
		c.MaxPendingBytes = 4 << 20
	}
	if c.ReadLimit <= 0 {
		c.ReadLimit = 60 * time.Second
	}
	if c.WriteLimit <= 0 {
		c.WriteLimit = 10 * time.Second
	}
	if c.PingInterval <= 0 {
		c.PingInterval = 30 * time.Second
	}
	return c
}

type Router struct {
	backend     Backend
	cfg         Config
	mu          sync.RWMutex // publish is exclusive with REQ registration + query
	clientsMu   sync.Mutex
	clients     map[*client]struct{}
	listenersMu sync.Mutex
	listeners   map[*listener]struct{}
	closed      chan struct{}
	closeOnce   sync.Once
	clientsWG   sync.WaitGroup
}

type listener struct{ fn func(event.Event) }

func New(backend Backend, cfg Config) *Router {
	return &Router{backend: backend, cfg: cfg.withDefaults(), clients: make(map[*client]struct{}), listeners: make(map[*listener]struct{}), closed: make(chan struct{})}
}

// Listen registers an in-process observer of live fan-out. It sees every
// event delivered to websocket subscribers, before the reader's own access
// check. The callback runs under the publish fence and must not block; the
// returned function removes it.
func (r *Router) Listen(fn func(event.Event)) func() {
	l := &listener{fn: fn}
	r.listenersMu.Lock()
	r.listeners[l] = struct{}{}
	r.listenersMu.Unlock()
	return func() {
		r.listenersMu.Lock()
		delete(r.listeners, l)
		r.listenersMu.Unlock()
	}
}

func (r *Router) HandleHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	conn, err := websocket.Accept(w, req, &websocket.AcceptOptions{OriginPatterns: r.cfg.OriginPatterns})
	if err != nil {
		return
	}
	c := newClient(r, conn, req)
	r.clientsMu.Lock()
	select {
	case <-r.closed:
		r.clientsMu.Unlock()
		_ = conn.Close(websocket.StatusGoingAway, "relay shutting down")
		return
	default:
	}
	r.clients[c] = struct{}{}
	if r.cfg.OnConnection != nil {
		r.cfg.OnConnection(1)
	}
	r.clientsWG.Add(1)
	r.clientsMu.Unlock()
	go c.run()
}

// Publish is the shared ingress path for websocket EVENT and HTTP-generated
// events. It applies the same historical/live fence and fan-out rules.
func (r *Router) Publish(ctx context.Context, e event.Event, s Session) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reason, err := r.backend.Publish(ctx, e, s)
	if err == nil && !duplicateReason(reason) {
		if accessEvent(e) {
			r.closeSubscriptionsLocked("blocked: relay access policy changed; subscribe again")
		}
		r.fanout(e)
	}
	return reason, err
}

func (r *Router) publishClient(c *client, e event.Event, s Session) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reason, err := r.backend.Publish(c.ctx, e, s)
	if err != nil {
		return reason, err
	}
	// Queue the sender's OK before any live EVENT fanout on that socket.
	c.replyOK(e.ID, true, reason)
	if accessEvent(e) {
		c.r.closeSubscriptionsLocked("blocked: relay access policy changed; subscribe again")
	}
	if reason == "" {
		r.fanout(e)
	}
	return reason, nil
}

// BroadcastGenerated delivers an event already durably accepted by an
// out-of-band producer.
func (r *Router) BroadcastGenerated(e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fanout(e)
}

// CommitGenerated runs an imported or server-generated event commit under the
// same historical/live fence as client EVENT publication. The callback must
// durably commit the event before returning; a returned error suppresses
// fanout. This prevents a REQ snapshot from racing an out-of-band broadcast.
func (r *Router) CommitGenerated(ctx context.Context, commit func(context.Context) (event.Event, error)) (event.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, err := commit(ctx)
	if err == nil && e.ID != "" {
		r.fanout(e)
	}
	return e, err
}

func (r *Router) Close(ctx context.Context) error {
	r.closeOnce.Do(func() { close(r.closed) })
	r.clientsMu.Lock()
	cs := make([]*client, 0, len(r.clients))
	for c := range r.clients {
		cs = append(cs, c)
	}
	r.clientsMu.Unlock()
	for _, c := range cs {
		c.close(websocket.StatusGoingAway, "relay shutting down")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.clientsDone():
		return nil
	}
}

// CloseSubscriptions invalidates all live subscriptions after a policy
// revision. Existing sockets remain usable for AUTH and a subsequent REQ.
func (r *Router) CloseSubscriptions(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeSubscriptionsLocked(reason)
}

// ApplyACLChange serializes an access-control mutation with REQ snapshots,
// NEG sessions, and live publication. A successful mutation invalidates both
// subscription families before another transport operation can begin.
func (r *Router) ApplyACLChange(ctx context.Context, change func(context.Context) (any, error), reason string) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result, err := change(ctx)
	if err == nil {
		r.closeSubscriptionsLocked(reason)
	}
	return result, err
}

func (r *Router) closeSubscriptionsLocked(reason string) {
	if reason == "" {
		reason = "blocked: relay policy changed"
	}
	r.clientsMu.Lock()
	cs := make([]*client, 0, len(r.clients))
	for c := range r.clients {
		cs = append(cs, c)
	}
	r.clientsMu.Unlock()
	for _, c := range cs {
		c.mu.Lock()
		ids := make([]string, 0, len(c.subs))
		for id := range c.subs {
			ids = append(ids, id)
			delete(c.subs, id)
		}
		syncIDs := make([]string, 0, len(c.syncs))
		for id := range c.syncs {
			syncIDs = append(syncIDs, id)
			delete(c.syncs, id)
		}
		c.mu.Unlock()
		for _, id := range ids {
			if r.cfg.OnSubscription != nil {
				r.cfg.OnSubscription(-1)
			}
			b, _ := json.Marshal([]any{"CLOSED", id, reason})
			_ = c.enqueueBytes(b)
		}
		for _, id := range syncIDs {
			b, _ := json.Marshal([]any{"NEG-CLOSE", id})
			_ = c.enqueueBytes(b)
		}
	}
	if p, ok := r.backend.(PolicyChanges); ok {
		p.CloseSubscriptions(Session{RelayURL: r.cfg.RelayURL})
	}
}

func (r *Router) clientsDone() <-chan struct{} {
	done := make(chan struct{})
	go func() { r.clientsWG.Wait(); close(done) }()
	return done
}

type subscription struct{ filters []event.Filter }
type outbound struct {
	data []byte
	size int
}

type client struct {
	r         *Router
	conn      *websocket.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	session   Session
	challenge string
	subs      map[string]subscription
	syncs     map[string]*syncprotocol.Session
	queue     []outbound
	pending   int
	wake      chan struct{}
	done      chan struct{}
	once      sync.Once
	wg        sync.WaitGroup
}

func newClient(r *Router, conn *websocket.Conn, req *http.Request) *client {
	ctx, cancel := context.WithCancel(context.Background())
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	relayURL := r.cfg.RelayURL
	if r.cfg.RequestRelayURL != nil {
		if requested := strings.TrimSpace(r.cfg.RequestRelayURL(req)); requested != "" {
			relayURL = requested
		}
	}
	return &client{r: r, conn: conn, ctx: ctx, cancel: cancel, challenge: hex.EncodeToString(b), subs: make(map[string]subscription), syncs: make(map[string]*syncprotocol.Session), wake: make(chan struct{}, 1), done: make(chan struct{}), session: Session{RelayURL: relayURL, RemoteIP: remoteIP(req.RemoteAddr)}}
}

func (c *client) run() {
	defer c.r.clientsWG.Done()
	c.wg.Add(1)
	go c.writer()
	_ = c.enqueue(fmt.Sprintf(`["AUTH",%q]`, c.challenge))
	c.readLoop()
	c.close(websocket.StatusNormalClosure, "closed")
	c.wg.Wait()
}

func (c *client) readLoop() {
	defer c.conn.CloseNow()
	readLimit := c.r.cfg.MaxMessageBytes
	if readLimit == 0 {
		readLimit = -1
	}
	c.conn.SetReadLimit(readLimit)
	for {
		// Read contexts must live for the connection. A per-message timeout
		// measures time between application messages, so it closes a healthy
		// idle session even when the websocket ping/pong control traffic is
		// flowing. The writer owns protocol keepalive; cancellation closes a
		// connection during shutdown.
		typ, data, err := c.conn.Read(c.ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			_ = c.enqueue(`["NOTICE","error: binary frames are not supported"]`)
			continue
		}
		if c.r.cfg.MaxMessageBytes > 0 && int64(len(data)) > c.r.cfg.MaxMessageBytes {
			_ = c.enqueue(`["NOTICE","error: message too large"]`)
			continue
		}
		var raw []json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil || len(raw) == 0 {
			_ = c.enqueue(`["NOTICE","error: could not parse message"]`)
			continue
		}
		var typName string
		if json.Unmarshal(raw[0], &typName) != nil {
			_ = c.enqueue(`["NOTICE","error: message type must be a string"]`)
			continue
		}
		c.dispatch(typName, raw[1:])
	}
}

func (c *client) dispatch(kind string, args []json.RawMessage) {
	switch kind {
	case "EVENT":
		c.handleEvent(args)
	case "REQ", "COUNT":
		c.handleRequest(kind, args)
	case "CLOSE":
		c.handleClose(args)
	case "AUTH":
		c.handleAuth(args)
	case "NEG-OPEN", "NEG-MSG", "NEG-CLOSE":
		c.handleSync(kind, args)
	default:
		_ = c.enqueue(fmt.Sprintf(`["NOTICE","error: unknown message type %s"]`, kind))
	}
}

func (c *client) handleSync(kind string, args []json.RawMessage) {
	if len(args) < 1 {
		_ = c.enqueue(fmt.Sprintf(`["NEG-ERR","",%q]`, "error: "+kind+" needs a subscription id"))
		return
	}
	var id string
	if json.Unmarshal(args[0], &id) != nil || id == "" || len(id) > 64 {
		_ = c.enqueue(`["NEG-ERR","","error: bad subscription id"]`)
		return
	}
	if kind == "NEG-CLOSE" {
		c.mu.Lock()
		delete(c.syncs, id)
		c.mu.Unlock()
		return
	}
	if kind == "NEG-OPEN" {
		if len(args) < 3 {
			c.syncError(id, "error: NEG-OPEN needs a filter and a message")
			return
		}
		f, err := event.ParseFilter(args[1])
		if err != nil {
			c.syncError(id, "invalid: bad filter: "+err.Error())
			return
		}
		var message string
		if json.Unmarshal(args[2], &message) != nil {
			c.syncError(id, "invalid: message must be hex")
			return
		}
		if _, err := hex.DecodeString(message); err != nil {
			c.syncError(id, "invalid: message must be hex")
			return
		}
		b, ok := c.r.backend.(SyncBackend)
		if !ok {
			c.syncError(id, "unsupported: sync is switched off on this relay")
			return
		}
		c.r.mu.RLock()
		items, err := b.Sync(c.ctx, f, c.snapshot())
		if err == nil {
			var sess *syncprotocol.Session
			sess, err = syncprotocol.NewSession(items, false)
			if err == nil {
				var result syncprotocol.Result
				result, err = sess.Reconcile(message)
				if err == nil {
					if result.Response == "" {
						_ = c.enqueue(fmt.Sprintf(`["NEG-CLOSE",%q]`, id))
					} else {
						c.mu.Lock()
						c.syncs[id] = sess
						c.mu.Unlock()
						c.sendSyncMessage(id, result.Response)
					}
				}
			}
		}
		c.r.mu.RUnlock()
		if err != nil {
			c.syncError(id, syncErrorReason(err))
		}
		return
	}
	if len(args) < 2 {
		c.syncError(id, "error: NEG-MSG needs a message")
		return
	}
	var message string
	if json.Unmarshal(args[1], &message) != nil {
		c.syncError(id, "invalid: message must be hex")
		return
	}
	if _, err := hex.DecodeString(message); err != nil {
		c.syncError(id, "invalid: message must be hex")
		return
	}
	c.mu.Lock()
	sess := c.syncs[id]
	c.mu.Unlock()
	if sess == nil {
		c.syncError(id, "closed: no such sync, send NEG-OPEN first")
		return
	}
	result, err := sess.Reconcile(message)
	if err != nil {
		c.mu.Lock()
		delete(c.syncs, id)
		c.mu.Unlock()
		c.syncError(id, "invalid: "+err.Error())
		return
	}
	if result.Response == "" {
		c.mu.Lock()
		delete(c.syncs, id)
		c.mu.Unlock()
		_ = c.enqueue(fmt.Sprintf(`["NEG-CLOSE",%q]`, id))
	} else {
		c.sendSyncMessage(id, result.Response)
	}
}

func (c *client) sendSyncMessage(id, message string) {
	b, _ := json.Marshal([]any{"NEG-MSG", id, message})
	_ = c.enqueueBytes(b)
}

func (c *client) syncError(id, reason string) {
	b, _ := json.Marshal([]any{"NEG-ERR", id, reason})
	_ = c.enqueueBytes(b)
}

func (c *client) handleEvent(args []json.RawMessage) {
	if len(args) < 1 {
		_ = c.enqueue(`["NOTICE","error: EVENT needs an event"]`)
		return
	}
	providedID := rawEventID(args[0])
	e, err := event.Parse(args[0])
	if err != nil {
		c.replyOK(providedID, false, err.Error())
		return
	}
	if e.Kind == event.KIND_AUTH {
		c.replyOK(e.ID, false, "blocked: AUTH events use AUTH")
		return
	}
	reason, err := c.r.publishClient(c, e, c.snapshot())
	if err != nil {
		c.replyOK(e.ID, false, err.Error())
		return
	}
	_ = reason
}

func (c *client) handleRequest(kind string, args []json.RawMessage) {
	if len(args) < 2 {
		_ = c.enqueue(fmt.Sprintf(`["NOTICE",%q]`, "error: "+kind+" needs an id and at least one filter"))
		return
	}
	var id string
	if json.Unmarshal(args[0], &id) != nil || id == "" || len(id) > 64 {
		_ = c.enqueue(`["NOTICE","error: bad subscription id"]`)
		return
	}
	filters := make([]event.Filter, 0, len(args)-1)
	for _, raw := range args[1:] {
		f, err := event.ParseFilter(raw)
		if err != nil {
			c.closedSub(id, "invalid: bad filter: "+err.Error())
			return
		}
		filters = append(filters, f)
	}
	if kind == "COUNT" {
		c.handleCount(id, filters)
		return
	}
	c.r.mu.Lock()
	c.mu.Lock()
	_, replaced := c.subs[id]
	c.subs[id] = subscription{filters: filters}
	if !replaced && c.r.cfg.OnSubscription != nil {
		c.r.cfg.OnSubscription(1)
	}
	c.mu.Unlock()
	var hints []string
	var events []event.Event
	var err error
	if hinted, ok := c.r.backend.(QueryHints); ok {
		events, hints, err = hinted.QueryHints(c.ctx, filters, c.snapshot())
	} else {
		events, err = c.r.backend.Query(c.ctx, filters, c.snapshot())
	}
	if err == nil {
		for _, e := range events {
			matched := false
			for _, f := range filters {
				if event.Matches(f, e) && c.canRead(e, &f) {
					matched = true
					break
				}
			}
			if matched {
				if !c.sendEvent(id, e) {
					err = errors.New("slow consumer")
					break
				}
			}
		}
	}
	if err == nil {
		if len(hints) > 0 {
			_ = c.enqueue(fmt.Sprintf(`["EOSE",%q,%s]`, id, mustJSON(hints)))
		} else {
			_ = c.enqueue(fmt.Sprintf(`["EOSE",%q]`, id))
		}
	}
	c.r.mu.Unlock()
	if err != nil {
		c.closedSub(id, queryErrorReason(err))
	}
}

func (c *client) handleCount(id string, filters []event.Filter) {
	c.r.mu.RLock()
	value, err := c.r.backend.Count(c.ctx, filters, c.snapshot())
	c.r.mu.RUnlock()
	if err != nil {
		c.closedSub(id, "error: count failed: "+err.Error())
		return
	}
	_ = c.enqueue(fmt.Sprintf(`["COUNT",%q,%s]`, id, mustJSON(value)))
}

func (c *client) handleClose(args []json.RawMessage) {
	if len(args) > 0 {
		var id string
		if json.Unmarshal(args[0], &id) == nil {
			c.mu.Lock()
			_, existed := c.subs[id]
			delete(c.subs, id)
			c.mu.Unlock()
			if existed && c.r.cfg.OnSubscription != nil {
				c.r.cfg.OnSubscription(-1)
			}
		}
	}
}

func (c *client) handleAuth(args []json.RawMessage) {
	if len(args) < 1 {
		c.replyOK("", false, "invalid: AUTH needs an event")
		return
	}
	providedID := rawEventID(args[0])
	e, err := event.Parse(args[0])
	if err != nil {
		c.replyOK(providedID, false, err.Error())
		return
	}
	if e.Kind != event.KIND_AUTH {
		c.replyOK(e.ID, false, "invalid: auth event must be kind 22242")
		return
	}
	if abs(time.Now().Unix()-e.CreatedAt) > 600 {
		c.replyOK(e.ID, false, "invalid: auth event created_at is too far from the current time")
		return
	}
	if event.Tag(e, "challenge") != c.challenge {
		c.replyOK(e.ID, false, "invalid: auth challenge does not match")
		return
	}
	if c.session.RelayURL != "" && normalizeAuthRelay(event.Tag(e, "relay")) != normalizeAuthRelay(c.session.RelayURL) {
		c.replyOK(e.ID, false, "invalid: auth relay tag does not name this relay")
		return
	}
	candidate := c.snapshot()
	seen := false
	for _, key := range candidate.PubKeys {
		if key == e.PubKey {
			seen = true
			break
		}
	}
	if !seen {
		candidate.PubKeys = append(candidate.PubKeys, e.PubKey)
	}
	if c.r.cfg.OnAuthenticate != nil {
		if err := c.r.cfg.OnAuthenticate(c.ctx, candidate); err != nil {
			c.replyOK(e.ID, false, "error: authentication bookkeeping failed: "+err.Error())
			return
		}
	}
	if !seen {
		c.mu.Lock()
		c.session.PubKeys = append(c.session.PubKeys, e.PubKey)
		c.mu.Unlock()
	}
	c.replyOK(e.ID, true, "")
}

func (r *Router) fanout(e event.Event) {
	r.clientsMu.Lock()
	cs := make([]*client, 0, len(r.clients))
	for x := range r.clients {
		cs = append(cs, x)
	}
	r.clientsMu.Unlock()
	for _, x := range cs {
		x.mu.Lock()
		subs := make(map[string]subscription, len(x.subs))
		for id, s := range x.subs {
			subs[id] = s
		}
		x.mu.Unlock()
		for id, s := range subs {
			for _, f := range s.filters {
				if event.Matches(f, e) && x.canRead(e, &f) {
					if !x.sendEvent(id, e) {
						x.close(websocket.StatusPolicyViolation, "slow consumer")
					}
					break
				}
			}
		}
	}
	r.listenersMu.Lock()
	ls := make([]*listener, 0, len(r.listeners))
	for l := range r.listeners {
		ls = append(ls, l)
	}
	r.listenersMu.Unlock()
	for _, l := range ls {
		l.fn(e)
	}
}

func (c *client) canRead(e event.Event, f *event.Filter) bool {
	if x, ok := c.r.backend.(FilterReader); ok {
		return x.CanReadFilter(e, c.snapshot(), f)
	}
	if x, ok := c.r.backend.(Reader); ok {
		return x.CanRead(e, c.snapshot())
	}
	return true
}
func (c *client) snapshot() Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.session
	s.PubKeys = append([]string(nil), s.PubKeys...)
	return s
}
func (c *client) sendEvent(id string, e event.Event) bool {
	b, _ := json.Marshal([]any{"EVENT", id, e})
	return c.enqueueBytes(b) == nil
}
func (c *client) replyOK(id string, ok bool, reason string) {
	b, _ := json.Marshal([]any{"OK", id, ok, reason})
	_ = c.enqueueBytes(b)
}
func (c *client) closedSub(id, reason string) {
	c.mu.Lock()
	delete(c.subs, id)
	c.mu.Unlock()
	b, _ := json.Marshal([]any{"CLOSED", id, reason})
	_ = c.enqueueBytes(b)
}
func (c *client) enqueue(s string) error { return c.enqueueBytes([]byte(s)) }
func (c *client) enqueueBytes(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	if c.pending+len(b) > c.r.cfg.MaxPendingBytes {
		return errors.New("slow consumer")
	}
	c.queue = append(c.queue, outbound{data: append([]byte(nil), b...), size: len(b)})
	c.pending += len(b)
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return nil
}

func (c *client) writer() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.r.cfg.PingInterval)
	defer ticker.Stop()
	for {
		c.mu.Lock()
		if len(c.queue) == 0 {
			c.mu.Unlock()
			select {
			case <-c.wake:
			case <-c.done:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(c.ctx, c.r.cfg.WriteLimit)
				err := c.conn.Ping(ctx)
				cancel()
				if err != nil {
					return
				}
			}
			continue
		}
		item := c.queue[0]
		c.queue = c.queue[1:]
		c.pending -= item.size
		c.mu.Unlock()
		ctx, cancel := context.WithTimeout(c.ctx, c.r.cfg.WriteLimit)
		err := c.conn.Write(ctx, websocket.MessageText, item.data)
		cancel()
		if err != nil {
			return
		}
	}
}
func (c *client) close(code websocket.StatusCode, reason string) {
	c.once.Do(func() {
		close(c.done)
		c.cancel()
		_ = c.conn.Close(code, reason)
		c.r.clientsMu.Lock()
		delete(c.r.clients, c)
		c.r.clientsMu.Unlock()
		if c.r.cfg.OnConnection != nil {
			c.r.cfg.OnConnection(-1)
		}
		c.mu.Lock()
		subscriptions := len(c.subs)
		c.subs = make(map[string]subscription)
		c.mu.Unlock()
		if c.r.cfg.OnSubscription != nil && subscriptions > 0 {
			c.r.cfg.OnSubscription(-subscriptions)
		}
	})
}
func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	if len(b) == 0 {
		return "null"
	}
	return string(b)
}
func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
func normalizeRelay(v string) string {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func normalizeAuthRelay(v string) string {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" {
		host += ":" + port
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	if path == "" {
		path = "/"
	}
	return strings.ToLower(u.Scheme) + "://" + host + path
}

func remoteIP(address string) string {
	if host, _, err := net.SplitHostPort(strings.TrimSpace(address)); err == nil {
		return host
	}
	if ip := net.ParseIP(strings.TrimSpace(address)); ip != nil {
		return ip.String()
	}
	return strings.TrimSpace(address)
}

func rawEventID(raw json.RawMessage) string {
	var probe struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return ""
	}
	return probe.ID
}

func queryErrorReason(err error) string {
	if err == nil {
		return ""
	}
	if knownProtocolReason(err.Error()) {
		return err.Error()
	}
	return "error: query failed: " + err.Error()
}

func syncErrorReason(err error) string {
	if err == nil {
		return ""
	}
	if knownProtocolReason(err.Error()) {
		return err.Error()
	}
	return "error: sync failed: " + err.Error()
}

func knownProtocolReason(reason string) bool {
	for _, prefix := range []string{"auth-required:", "restricted:", "blocked:", "invalid:", "unsupported:"} {
		if strings.HasPrefix(reason, prefix) {
			return true
		}
	}
	return false
}

func accessEvent(e event.Event) bool {
	if e.Kind == event.KIND_NIP43_JOIN && event.Tag(e, "claim") == "" {
		// An access request waits for review and changes nobody's access.
		return false
	}
	switch e.Kind {
	case event.KIND_REPORT, event.KIND_VANISH, event.KIND_JOIN, event.KIND_LEAVE,
		event.KIND_NIP43_JOIN, event.KIND_NIP43_LEAVE, event.KIND_PUT_USER,
		event.KIND_REMOVE_USER, event.KIND_DELETE_EVENT, event.KIND_DELETE_GROUP:
		return true
	default:
		return false
	}
}

func duplicateReason(reason string) bool {
	return strings.HasPrefix(reason, "duplicate:")
}

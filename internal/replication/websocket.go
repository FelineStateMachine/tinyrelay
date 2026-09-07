package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/syncprotocol"
	"github.com/coder/websocket"
)

type Socket interface {
	Read(context.Context) ([]byte, error)
	Write(context.Context, []byte) error
	Close(error) error
}

type Dialer interface {
	Dial(context.Context, string) (Socket, *http.Response, error)
}

type IPResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type WebsocketDialer struct {
	AllowPrivate    bool
	MaxMessageBytes int64
	Resolver        IPResolver
	HTTPClient      *http.Client
	// Authenticate answers a NIP-42 challenge when configured for an
	// operator-trusted private peer. Public relays remain unauthenticated.
	Authenticate func(context.Context, string, string) (event.Event, error)
}

func (d WebsocketDialer) Dial(ctx context.Context, target string) (Socket, *http.Response, error) {
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, nil, errors.New("replication: invalid websocket URL")
	}
	if d.MaxMessageBytes < 0 {
		d.MaxMessageBytes = 8 << 20
	}
	resolver := d.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if err := validateHost(ctx, parsed.Hostname(), resolver, d.AllowPrivate); err != nil {
		return nil, nil, err
	}
	client := d.client(resolver, d.AllowPrivate)
	conn, response, err := websocket.Dial(ctx, target, &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		return nil, response, err
	}
	limit := d.MaxMessageBytes
	if limit == 0 {
		limit = -1
	}
	conn.SetReadLimit(limit)
	if d.Authenticate != nil {
		if err := authenticateSocket(ctx, conn, target, d.Authenticate); err != nil {
			_ = conn.Close(websocket.StatusPolicyViolation, "authentication failed")
			return nil, response, err
		}
	}
	return coderSocket{conn: conn}, response, nil
}

func authenticateSocket(ctx context.Context, conn *websocket.Conn, target string, sign func(context.Context, string, string) (event.Event, error)) error {
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		return fmt.Errorf("replication: NIP-42 challenge: %w", err)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || len(raw) < 2 {
		return errors.New("replication: invalid NIP-42 challenge")
	}
	var kind, challenge string
	if err := json.Unmarshal(raw[0], &kind); err != nil || kind != "AUTH" || json.Unmarshal(raw[1], &challenge) != nil || challenge == "" {
		return errors.New("replication: peer did not issue NIP-42 challenge")
	}
	proof, err := sign(ctx, target, challenge)
	if err != nil {
		return err
	}
	message, err := json.Marshal([]any{"AUTH", proof})
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, message)
}

func (d WebsocketDialer) client(resolver IPResolver, allowPrivate bool) *http.Client {
	base := d.HTTPClient
	if base == nil {
		base = &http.Client{}
	}
	if base.Transport != nil {
		if _, standard := base.Transport.(*http.Transport); !standard {
			// Nonstandard transports are an explicit test/integration injection;
			// the caller owns their proxy and address policy.
			return base
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if base.Transport != nil {
		if source, ok := base.Transport.(*http.Transport); ok {
			transport = source.Clone()
		}
	}
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if err := validateHost(ctx, host, resolver, allowPrivate); err != nil {
			return nil, err
		}
		ips, err := resolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			if err == nil {
				err = errors.New("no address")
			}
			return nil, fmt.Errorf("replication: resolve %s: %w", host, err)
		}
		var last error
		for _, candidate := range ips {
			if err := validateIP(candidate.IP, allowPrivate); err != nil {
				return nil, err
			}
			conn, dialErr := (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			last = dialErr
		}
		return nil, last
	}
	client := *base
	client.Transport = transport
	oldRedirect := base.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := validateURL(req.URL); err != nil {
			return err
		}
		if err := validateHost(req.Context(), req.URL.Hostname(), resolver, allowPrivate); err != nil {
			return err
		}
		if len(via) >= 10 {
			return errors.New("replication: stopped after 10 redirects")
		}
		if oldRedirect != nil {
			return oldRedirect(req, via)
		}
		return nil
	}
	return &client
}

func validateURL(target *url.URL) error {
	if target == nil || (target.Scheme != "ws" && target.Scheme != "wss" && target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.User != nil || target.Fragment != "" {
		return errors.New("replication: invalid websocket redirect")
	}
	return nil
}

func validateHost(ctx context.Context, host string, resolver IPResolver, allowPrivate bool) error {
	if host == "" {
		return errors.New("replication: websocket host is empty")
	}
	if ip := net.ParseIP(host); ip != nil {
		return validateIP(ip, allowPrivate)
	}
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("replication: resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("replication: resolve %s: no address", host)
	}
	for _, item := range ips {
		if err := validateIP(item.IP, allowPrivate); err != nil {
			return err
		}
	}
	return nil
}

func validateIP(ip net.IP, allowPrivate bool) error {
	if allowPrivate {
		return nil
	}
	if unusableIP(ip) {
		return fmt.Errorf("replication: private or local relay address %s is blocked", ip)
	}
	return nil
}

func unusableIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || ip.Equal(net.ParseIP("100.100.100.100")) {
		return true
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	// CGNAT/Tailscale and documentation/reserved IPv4 ranges are not public
	// relay destinations unless the operator explicitly permits private peers.
	if v4[0] == 100 && v4[1]&0xc0 == 64 {
		return true
	}
	if v4[0] == 192 && v4[1] == 0 && v4[2] == 0 {
		return true
	}
	if v4[0] == 198 && v4[1] >= 18 && v4[1] <= 19 {
		return true
	}
	return v4[0] >= 240
}

type coderSocket struct{ conn *websocket.Conn }

func (s coderSocket) Read(ctx context.Context) ([]byte, error) {
	_, data, err := s.conn.Read(ctx)
	return data, err
}

func (s coderSocket) Write(ctx context.Context, data []byte) error {
	return s.conn.Write(ctx, websocket.MessageText, data)
}

func (s coderSocket) Close(err error) error {
	code := websocket.StatusNormalClosure
	if err != nil {
		code = websocket.StatusInternalError
	}
	return s.conn.Close(code, "replication complete")
}

type NostrTransport struct {
	Dialer   Dialer
	Timeout  time.Duration
	sequence atomic.Uint64
}

// QueryNegentropy reconciles a local index with a remote relay, then fetches
// only IDs the local side lacks. It is optional at the PullTransport boundary
// so ordinary REQ remains a compatible fallback.
func (t *NostrTransport) QueryNegentropy(ctx context.Context, target string, filter event.Filter, local []syncprotocol.Item) ([]event.Event, error) {
	session, err := syncprotocol.NewSession(local, true)
	if err != nil {
		return nil, err
	}
	start, err := session.Start()
	if err != nil {
		return nil, err
	}
	socket, _, err := t.dial(ctx, target)
	if err != nil {
		return nil, err
	}
	defer socket.Close(nil)
	id := fmt.Sprintf("tiny-neg-%d", t.sequence.Add(1))
	filter.Tags = ensureTags(filter.Tags)
	open, err := json.Marshal([]any{"NEG-OPEN", id, filter, start})
	if err != nil {
		return nil, err
	}
	if err := socket.Write(ctx, open); err != nil {
		return nil, err
	}
	var needed []string
	deadline, cancel := context.WithTimeout(ctx, t.timeout())
	defer cancel()
	for {
		data, err := socket.Read(deadline)
		if err != nil {
			return nil, err
		}
		var raw []json.RawMessage
		if json.Unmarshal(data, &raw) != nil || len(raw) < 2 {
			continue
		}
		var kind, messageID, body string
		_ = json.Unmarshal(raw[0], &kind)
		_ = json.Unmarshal(raw[1], &messageID)
		if len(raw) > 2 {
			_ = json.Unmarshal(raw[2], &body)
		}
		if messageID != id {
			continue
		}
		// A responder may close immediately when its authorized snapshot is
		// already identical. Treat that as a completed empty reconciliation;
		// waiting for a NEG-MSG that will never arrive turns a successful NIP-77
		// no-op sync into a timeout.
		if kind == "NEG-CLOSE" {
			break
		}
		if kind == "NEG-ERR" {
			return nil, errors.New("replication: remote negentropy rejected")
		}
		if kind != "NEG-MSG" {
			continue
		}
		result, err := session.Reconcile(body)
		if err != nil {
			return nil, err
		}
		needed = append(needed, result.Need...)
		if result.Done {
			closeMessage, _ := json.Marshal([]any{"NEG-CLOSE", id})
			_ = socket.Write(ctx, closeMessage)
			break
		}
		reply, err := json.Marshal([]any{"NEG-MSG", id, result.Response})
		if err != nil {
			return nil, err
		}
		if err := socket.Write(ctx, reply); err != nil {
			return nil, err
		}
	}
	if len(needed) == 0 {
		return nil, nil
	}
	return t.queryNegentropyIDs(ctx, target, filter, needed)
}

const negentropyFetchBatch = 500

func (t *NostrTransport) queryNegentropyIDs(ctx context.Context, target string, filter event.Filter, ids []string) ([]event.Event, error) {
	result := make([]event.Event, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for start := 0; start < len(ids); start += negentropyFetchBatch {
		end := start + negentropyFetchBatch
		if end > len(ids) {
			end = len(ids)
		}
		batch := append([]string(nil), ids[start:end]...)
		request := filter
		request.IDs = batch
		limit := len(batch)
		request.Limit = &limit
		events, err := t.Query(ctx, target, request)
		if err != nil {
			return nil, err
		}
		allowed := make(map[string]struct{}, len(batch))
		for _, id := range batch {
			allowed[id] = struct{}{}
		}
		for _, candidate := range events {
			if _, ok := allowed[candidate.ID]; !ok {
				continue
			}
			if !event.Matches(filter, candidate) {
				continue
			}
			if _, ok := seen[candidate.ID]; ok {
				continue
			}
			seen[candidate.ID] = struct{}{}
			result = append(result, candidate)
		}
	}
	return result, nil
}

func (t *NostrTransport) timeout() time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	return 20 * time.Second
}

func (t *NostrTransport) Query(ctx context.Context, target string, filter event.Filter) ([]event.Event, error) {
	socket, _, err := t.dial(ctx, target)
	if err != nil {
		return nil, err
	}
	defer socket.Close(nil)
	sub := fmt.Sprintf("tiny-pull-%d", t.sequence.Add(1))
	filter.Tags = ensureTags(filter.Tags)
	message, err := json.Marshal([]any{"REQ", sub, filter})
	if err != nil {
		return nil, fmt.Errorf("encode query: %w", err)
	}
	if err := socket.Write(ctx, message); err != nil {
		return nil, err
	}
	return readEvents(ctx, socket, sub, t.timeout())
}

func (t *NostrTransport) Send(ctx context.Context, target string, e event.Event) (DeliveryResult, error) {
	socket, _, err := t.dial(ctx, target)
	if err != nil {
		return DeliveryResult{}, err
	}
	defer socket.Close(nil)
	message, err := json.Marshal([]any{"EVENT", e})
	if err != nil {
		return DeliveryResult{}, err
	}
	if err := socket.Write(ctx, message); err != nil {
		return DeliveryResult{}, err
	}
	deadline, cancel := context.WithTimeout(ctx, t.timeout())
	defer cancel()
	for {
		data, err := socket.Read(deadline)
		if err != nil {
			return DeliveryResult{}, err
		}
		var message []json.RawMessage
		if json.Unmarshal(data, &message) != nil || len(message) < 4 {
			continue
		}
		var kind, id, text string
		var accepted bool
		_ = json.Unmarshal(message[0], &kind)
		_ = json.Unmarshal(message[1], &id)
		_ = json.Unmarshal(message[2], &accepted)
		_ = json.Unmarshal(message[3], &text)
		if kind == "OK" && id == e.ID {
			return DeliveryResult{Accepted: accepted, Message: text}, nil
		}
	}
}

func (t *NostrTransport) dial(ctx context.Context, target string) (Socket, *http.Response, error) {
	dialer := t.Dialer
	if dialer == nil {
		dialer = WebsocketDialer{}
	}
	return dialer.Dial(ctx, target)
}

func readEvents(ctx context.Context, socket Socket, subscription string, timeout time.Duration) ([]event.Event, error) {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	items := make([]event.Event, 0)
	for {
		data, err := socket.Read(deadline)
		if err != nil {
			return nil, err
		}
		var raw []json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil || len(raw) < 2 {
			continue
		}
		var kind, id string
		_ = json.Unmarshal(raw[0], &kind)
		_ = json.Unmarshal(raw[1], &id)
		switch kind {
		case "EVENT":
			if id != subscription || len(raw) < 3 {
				continue
			}
			var item event.Event
			if json.Unmarshal(raw[2], &item) == nil && event.Validate(item) == nil {
				items = append(items, item)
			}
		case "EOSE":
			if id == subscription {
				return items, nil
			}
		case "CLOSED":
			if id == subscription {
				return nil, errors.New("replication: remote query closed")
			}
		case "AUTH":
			return nil, errors.New("replication: remote relay requires authentication")
		}
	}
}

func ensureTags(tags map[string][]string) map[string][]string {
	if tags != nil {
		return tags
	}
	return map[string][]string{}
}

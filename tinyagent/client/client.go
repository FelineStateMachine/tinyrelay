// Package client provides the native Tinyrelay transport used by tinyagent.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
	"github.com/coder/websocket"
)

const maxDownload = 32 << 20
const maxQueryEvents = 10000

type Client struct {
	BaseURL, Secret, PubKey string
	HTTP                    *http.Client
}
type Filter map[string]any
type UploadResult map[string]any

func New(base, secret string) (*Client, error) {
	u, err := normalizeBase(base)
	if err != nil {
		return nil, err
	}
	pub, err := nostr.PublicKey(secret)
	if err != nil {
		return nil, err
	}
	return &Client{BaseURL: u, Secret: secret, PubKey: pub, HTTP: &http.Client{CheckRedirect: rejectExternalRedirect}}, nil
}

func (c *Client) Identity() map[string]string { return map[string]string{"pubkey": c.PubKey} }

func (c *Client) Publish(ctx context.Context, e nostr.Event) (nostr.Event, error) {
	if e.CreatedAt == 0 {
		e.CreatedAt = time.Now().Unix()
	}
	if err := nostr.Sign(&e, c.Secret); err != nil {
		return nostr.Event{}, err
	}
	body, err := nostr.Canonical(e)
	if err != nil {
		return nostr.Event{}, err
	}
	result, err := c.doRaw(ctx, http.MethodPost, "/events", body)
	if err != nil {
		return nostr.Event{}, err
	}
	var receipt struct {
		Accepted *bool  `json:"accepted"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(result, &receipt); err != nil || receipt.Accepted == nil {
		return nostr.Event{}, errors.New("relay returned an invalid publish receipt")
	}
	if !*receipt.Accepted {
		if receipt.Message == "" {
			receipt.Message = "relay rejected event"
		}
		return nostr.Event{}, errors.New(receipt.Message)
	}
	return e, nil
}

func (c *Client) Request(ctx context.Context, requestPath, method string, body json.RawMessage) (any, error) {
	if method == "" {
		method = http.MethodGet
	}
	if !validMethod(method) {
		return nil, errors.New("invalid HTTP method")
	}
	b, err := c.doRaw(ctx, method, requestPath, body)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}

func (c *Client) Upload(ctx context.Context, room, filename, typ string, data []byte) (UploadResult, error) {
	if room == "" || len(data) == 0 || len(data) > maxDownload {
		return nil, errors.New("invalid upload")
	}
	p := "/rooms/" + url.PathEscape(room) + "/attachments?filename=" + url.QueryEscape(path.Base(filename))
	b, err := c.doRawWithHeaders(ctx, http.MethodPut, p, data, map[string]string{"Content-Type": typ, "X-Filename": filename})
	if err != nil {
		return nil, err
	}
	var out UploadResult
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("decode upload: %w", err)
	}
	return out, nil
}

func (c *Client) Download(ctx context.Context, requestPath string) (map[string]any, error) {
	b, typ, err := c.getBounded(ctx, requestPath)
	if err != nil {
		return nil, err
	}
	return map[string]any{"data": base64.StdEncoding.EncodeToString(b), "type": typ}, nil
}

func (c *Client) Query(ctx context.Context, filters []Filter) ([]nostr.Event, error) {
	return c.queryWebsocket(ctx, filters)
}

type EventSink func(string, nostr.Event)

func (c *Client) Subscribe(ctx context.Context, name string, filter Filter, sink EventSink) error {
	delay := time.Second
	for ctx.Err() == nil {
		err := c.subscribeOnce(ctx, name, filter, sink)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			delay = time.Second
		} else {
			delay = min(delay*2, 30*time.Second)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}

func (c *Client) subscribeOnce(ctx context.Context, name string, filter Filter, sink EventSink) error {
	wsURL, err := websocketURL(c.BaseURL)
	if err != nil {
		return err
	}
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	if err := writeFrame(ctx, conn, []any{"REQ", name, filter}); err != nil {
		return err
	}
	connected := false
	authRequired := false
	authenticated := false
	emitConnected := func() {
		if !connected && sink != nil {
			sink("connected", nostr.Event{})
			connected = true
		}
	}
	authID := ""
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var frame []json.RawMessage
		if json.Unmarshal(raw, &frame) != nil || len(frame) < 2 {
			continue
		}
		var kind string
		_ = json.Unmarshal(frame[0], &kind)
		switch kind {
		case "AUTH":
			authRequired = true
			var challenge string
			if json.Unmarshal(frame[1], &challenge) != nil {
				return errors.New("invalid AUTH challenge")
			}
			authRelay, relayErr := websocketURL(c.BaseURL)
			if relayErr != nil {
				return relayErr
			}
			auth := nostr.Event{Kind: 22242, CreatedAt: time.Now().Unix(), Tags: [][]string{{"relay", authRelay}, {"challenge", challenge}}}
			if err := nostr.Sign(&auth, c.Secret); err != nil {
				return err
			}
			if err := writeFrame(ctx, conn, []any{"AUTH", auth}); err != nil {
				return err
			}
			authID = auth.ID
			// Tiny may have already evaluated the unauthenticated REQ. Reissue it
			// after AUTH so private filters are evaluated in the authenticated session.
			if err := writeFrame(ctx, conn, []any{"REQ", name, filter}); err != nil {
				return err
			}
		case "OK":
			if len(frame) >= 3 {
				var id string
				var accepted bool
				_ = json.Unmarshal(frame[1], &id)
				_ = json.Unmarshal(frame[2], &accepted)
				if id == authID {
					if !accepted {
						return errors.New("relay rejected AUTH")
					}
					authenticated = true
				}
			}
		case "EVENT":
			if len(frame) != 3 {
				continue
			}
			var e nostr.Event
			if json.Unmarshal(frame[2], &e) != nil || nostr.Validate(e) != nil {
				continue
			}
			var sub string
			_ = json.Unmarshal(frame[1], &sub)
			if sink != nil && sub == name && eventMatches(e, filter) && (!authRequired || authenticated) {
				emitConnected()
				sink(sub, e)
			}
		case "EOSE":
			var sub string
			_ = json.Unmarshal(frame[1], &sub)
			if sub == name && (!authRequired || authenticated) {
				emitConnected()
			}
		case "CLOSED":
			var reason string
			if len(frame) > 2 {
				_ = json.Unmarshal(frame[2], &reason)
			}
			if strings.HasPrefix(reason, "auth-required:") && !authenticated {
				continue
			}
			return errors.New("subscription closed")
		}
	}
}

func (c *Client) queryWebsocket(ctx context.Context, filters []Filter) ([]nostr.Event, error) {
	wsURL, err := websocketURL(c.BaseURL)
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	name := "tinyagent-query"
	if err := writeQuery(ctx, conn, name, filters); err != nil {
		return nil, err
	}
	var out []nostr.Event
	awaitingAuth := false
	authID := ""
	for {
		_, raw, readErr := conn.Read(ctx)
		if readErr != nil {
			return nil, readErr
		}
		var frame []json.RawMessage
		if json.Unmarshal(raw, &frame) != nil || len(frame) < 2 {
			continue
		}
		var kind string
		_ = json.Unmarshal(frame[0], &kind)
		if kind == "AUTH" {
			var challenge string
			if json.Unmarshal(frame[1], &challenge) != nil {
				return nil, errors.New("invalid AUTH challenge")
			}
			authRelay, relayErr := websocketURL(c.BaseURL)
			if relayErr != nil {
				return nil, relayErr
			}
			auth := nostr.Event{Kind: 22242, CreatedAt: time.Now().Unix(), Tags: [][]string{{"relay", authRelay}, {"challenge", challenge}}}
			if err := nostr.Sign(&auth, c.Secret); err != nil {
				return nil, err
			}
			if err := writeFrame(ctx, conn, []any{"AUTH", auth}); err != nil {
				return nil, err
			}
			authID = auth.ID
			awaitingAuth = true
			if err := writeQuery(ctx, conn, name, filters); err != nil {
				return nil, err
			}
			continue
		}
		if kind == "OK" && len(frame) >= 3 {
			var id string
			var accepted bool
			_ = json.Unmarshal(frame[1], &id)
			_ = json.Unmarshal(frame[2], &accepted)
			if id == authID {
				if !accepted {
					return nil, errors.New("relay rejected AUTH")
				}
				awaitingAuth = false
			}
		}
		if kind == "EVENT" && len(frame) == 3 {
			var sub string
			if json.Unmarshal(frame[1], &sub) != nil || sub != name {
				continue
			}
			var e nostr.Event
			if json.Unmarshal(frame[2], &e) == nil && nostr.Validate(e) == nil && queryMatches(e, filters) {
				if len(out) >= maxQueryEvents {
					return nil, errors.New("query exceeds 10000 events")
				}
				out = append(out, e)
			}
		}
		if kind == "EOSE" {
			if awaitingAuth {
				continue
			}
			return out, nil
		}
		if kind == "CLOSED" {
			var reason string
			if len(frame) > 2 {
				_ = json.Unmarshal(frame[2], &reason)
			}
			if strings.HasPrefix(reason, "auth-required:") && awaitingAuth {
				continue
			}
			return nil, errors.New("query closed")
		}
	}
}

func (c *Client) doJSON(ctx context.Context, method, p string, body []byte, out any) error {
	raw, err := c.doRaw(ctx, method, p, body)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
func (c *Client) doRaw(ctx context.Context, method, p string, body []byte) ([]byte, error) {
	return c.doRawWithHeaders(ctx, method, p, body, map[string]string{"Content-Type": "application/json"})
}
func (c *Client) doRawWithHeaders(ctx context.Context, method, p string, body []byte, headers map[string]string) ([]byte, error) {
	u, err := c.requestURL(p)
	if err != nil {
		return nil, err
	}
	proof, err := c.proof(method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", proof)
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	if method == http.MethodPost && u.Path == "/manage/rpc" {
		req.Header.Set("Content-Type", "application/nostr+json+rpc")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDownload {
		return nil, errors.New("response exceeds 32 MiB")
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("relay HTTP %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func (c *Client) getBounded(ctx context.Context, p string) ([]byte, string, error) {
	u, err := c.requestURL(p)
	if err != nil {
		return nil, "", err
	}
	proof, err := c.proof(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", proof)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, "", err
	}
	if len(b) > maxDownload {
		return nil, "", errors.New("response exceeds 32 MiB")
	}
	if resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("relay HTTP %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return b, resp.Header.Get("Content-Type"), nil
}
func (c *Client) proof(method, raw string, body []byte) (string, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate request nonce: %w", err)
	}
	tags := [][]string{{"u", raw}, {"method", strings.ToUpper(method)}, {"nonce", hex.EncodeToString(nonce)}}
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		tags = append(tags, []string{"payload", hex.EncodeToString(sum[:])})
	}
	e := nostr.Event{Kind: 27235, CreatedAt: time.Now().Unix(), Tags: tags}
	if err := nostr.Sign(&e, c.Secret); err != nil {
		return "", err
	}
	b, err := nostr.Canonical(e)
	if err != nil {
		return "", err
	}
	return "Nostr " + base64.StdEncoding.EncodeToString(b), nil
}
func (c *Client) requestURL(p string) (*url.URL, error) {
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return nil, errors.New("path must be same-origin and absolute")
	}
	u, err := url.Parse(p)
	if err != nil || strings.Contains(p, "..") || strings.Contains(u.Path, "..") {
		return nil, errors.New("unsafe path")
	}
	base, _ := url.Parse(c.BaseURL)
	u.Scheme, u.Host = base.Scheme, base.Host
	if base.Path != "" && base.Path != "/" {
		u.Path = strings.TrimRight(base.Path, "/") + u.Path
	}
	return u, nil
}
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func rejectExternalRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 || req.URL.Scheme == via[0].URL.Scheme && req.URL.Host == via[0].URL.Host {
		return nil
	}
	return errors.New("external redirects are forbidden")
}
func normalizeBase(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "ws" && u.Scheme != "wss") {
		return "", errors.New("relay must be an http(s) or ws(s) URL")
	}
	if u.Scheme == "ws" {
		u.Scheme = "http"
	}
	if u.Scheme == "wss" {
		u.Scheme = "https"
	}
	return strings.TrimRight(u.String(), "/"), nil
}
func websocketURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", errors.New("invalid relay URL")
	}
	if u.Scheme == "http" {
		u.Scheme = "ws"
	} else if u.Scheme == "https" {
		u.Scheme = "wss"
	} else if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", errors.New("relay must be HTTP or WebSocket")
	}
	return u.String(), nil
}
func writeFrame(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}

func writeQuery(ctx context.Context, conn *websocket.Conn, name string, filters []Filter) error {
	args := make([]any, 2, len(filters)+2)
	args[0], args[1] = "REQ", name
	for _, filter := range filters {
		args = append(args, filter)
	}
	return writeFrame(ctx, conn, args)
}
func validateEvents(events []nostr.Event) ([]nostr.Event, error) {
	for _, e := range events {
		if err := nostr.Validate(e); err != nil {
			return nil, err
		}
	}
	return events, nil
}

func eventMatches(e nostr.Event, filter Filter) bool { return queryMatches(e, []Filter{filter}) }

func queryMatches(e nostr.Event, filters []Filter) bool {
	if len(filters) == 0 {
		return true
	}
	for _, filter := range filters {
		if filterMatches(e, filter) {
			return true
		}
	}
	return false
}

func filterMatches(e nostr.Event, filter Filter) bool {
	if !matchesStrings(filter["ids"], e.ID) || !matchesStrings(filter["authors"], e.PubKey) || !matchesInts(filter["kinds"], e.Kind) {
		return false
	}
	if n, ok := filterInt64(filter["since"]); ok && e.CreatedAt < n {
		return false
	}
	if n, ok := filterInt64(filter["until"]); ok && e.CreatedAt > n {
		return false
	}
	for key, value := range filter {
		if strings.HasPrefix(key, "#") && !matchesTag(e, key[1:], value) {
			return false
		}
	}
	return true
}

func matchesStrings(raw any, want string) bool {
	if raw == nil {
		return true
	}
	values, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func matchesInts(raw any, want int) bool {
	if raw == nil {
		return true
	}
	values, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, value := range values {
		if number, ok := value.(float64); ok && int(number) == want {
			return true
		}
	}
	return false
}

func filterInt64(raw any) (int64, bool) {
	number, ok := raw.(float64)
	return int64(number), ok
}

func matchesTag(e nostr.Event, name string, raw any) bool {
	values, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, tag := range e.Tags {
		if len(tag) < 2 || tag[0] != name {
			continue
		}
		for _, value := range values {
			if value == tag[1] {
				return true
			}
		}
	}
	return false
}
func validMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete || method == http.MethodHead
}
func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

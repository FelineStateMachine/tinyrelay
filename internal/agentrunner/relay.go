package agentrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/coder/websocket"
)

type RelayClient struct {
	URL, Secret, PubKey string
	Rooms               []string
	OnEvent             func(event.Event)
	OnReady             func()
	OnError             func(error)
	mu                  sync.Mutex
	conn                *websocket.Conn
	connection          context.Context
	pending             map[string]chan error
}

func (c *RelayClient) Run(ctx context.Context) error {
	parsed, err := url.Parse(c.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("agent relay must be a ws or wss URL")
	}
	c.PubKey, err = event.PublicKey(c.Secret)
	if err != nil {
		return err
	}
	delay := time.Second
	for ctx.Err() == nil {
		err = c.connect(ctx)
		if ctx.Err() != nil {
			break
		}
		if c.OnError != nil {
			c.OnError(err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, 30*time.Second)
	}
	return ctx.Err()
}
func (c *RelayClient) connect(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, c.URL, nil)
	if err != nil {
		return err
	}
	conn.SetReadLimit(1 << 20)
	scope, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.CloseNow()
	c.mu.Lock()
	c.conn = conn
	c.connection = scope
	c.pending = map[string]chan error{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
	}()
	if err := c.subscribe(scope, conn); err != nil {
		return err
	}
	authID := ""
	for {
		_, raw, err := conn.Read(scope)
		if err != nil {
			return err
		}
		var frame []json.RawMessage
		if json.Unmarshal(raw, &frame) != nil || len(frame) < 2 {
			continue
		}
		var kind string
		json.Unmarshal(frame[0], &kind)
		switch kind {
		case "AUTH":
			var challenge string
			if json.Unmarshal(frame[1], &challenge) != nil {
				continue
			}
			auth := event.Event{Kind: 22242, CreatedAt: time.Now().Unix(), Tags: [][]string{{"relay", c.URL}, {"challenge", challenge}}}
			if err := event.Sign(&auth, c.Secret); err != nil {
				return err
			}
			authID = auth.ID
			if err := writeRelay(scope, conn, []any{"AUTH", auth}); err != nil {
				return err
			}
		case "OK":
			if len(frame) < 3 {
				continue
			}
			var id string
			var ok bool
			var reason string
			json.Unmarshal(frame[1], &id)
			json.Unmarshal(frame[2], &ok)
			if len(frame) > 3 {
				json.Unmarshal(frame[3], &reason)
			}
			if id == authID {
				if !ok {
					return fmt.Errorf("agent authentication refused: %s", reason)
				}
				if err := c.subscribe(scope, conn); err != nil {
					return err
				}
				if c.OnReady != nil {
					c.OnReady()
				}
				continue
			}
			var result error
			if !ok && !strings.HasPrefix(reason, "duplicate:") {
				result = fmt.Errorf("relay refused event: %s", reason)
			}
			c.mu.Lock()
			wait := c.pending[id]
			c.mu.Unlock()
			if wait != nil {
				select {
				case wait <- result:
				default:
				}
			}
		case "EOSE":
			if c.OnReady != nil {
				c.OnReady()
			}
		case "EVENT":
			if len(frame) != 3 {
				continue
			}
			e, err := event.Parse(frame[2])
			if err != nil {
				continue
			}
			if c.OnEvent != nil {
				c.OnEvent(e)
			}
		case "CLOSED":
			if len(frame) > 2 {
				var reason string
				json.Unmarshal(frame[2], &reason)
				if !strings.HasPrefix(reason, "auth-required:") {
					return fmt.Errorf("agent subscription closed: %s", reason)
				}
			}
		}
	}
}
func (c *RelayClient) subscribe(ctx context.Context, conn *websocket.Conn) error {
	since := time.Now().Add(-5 * time.Minute).Unix()
	return writeRelay(ctx, conn, []any{"REQ", "tiny-agent", map[string]any{"kinds": []int{9, 11, 12}, "#h": c.Rooms, "#p": []string{c.PubKey}, "since": since}, map[string]any{"kinds": []int{7, 1111}, "#p": []string{c.PubKey}, "since": since}})
}
func writeRelay(ctx context.Context, conn *websocket.Conn, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, raw)
}
func (c *RelayClient) Publish(ctx context.Context, e event.Event) (event.Event, error) {
	if e.CreatedAt == 0 {
		e.CreatedAt = time.Now().Unix()
	}
	if err := event.Sign(&e, c.Secret); err != nil {
		return e, err
	}
	c.mu.Lock()
	conn, scope := c.conn, c.connection
	if conn == nil {
		c.mu.Unlock()
		return e, errors.New("agent relay is disconnected")
	}
	wait := make(chan error, 1)
	c.pending[e.ID] = wait
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, e.ID); c.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := writeRelay(ctx, conn, []any{"EVENT", e}); err != nil {
		return e, err
	}
	select {
	case err := <-wait:
		return e, err
	case <-ctx.Done():
		return e, ctx.Err()
	case <-scope.Done():
		return e, errors.New("agent relay disconnected before acknowledgement")
	}
}
func (c *RelayClient) Mention(e event.Event) (Mention, bool) {
	if e.PubKey == c.PubKey || !slices.Contains([]int{9, 11, 12}, e.Kind) || !slices.Contains(event.TagValues(e, "p"), c.PubKey) || !slices.Contains(c.Rooms, event.Tag(e, "h")) {
		return Mention{}, false
	}
	if e.CreatedAt > time.Now().Add(time.Minute).Unix() || e.CreatedAt < time.Now().Add(-24*time.Hour).Unix() || len(e.Content) > 64000 {
		return Mention{}, false
	}
	root := event.RoomReplyRoot(e)
	if root == "" {
		root = e.ID
	}
	return Mention{Room: event.Tag(e, "h"), EventID: e.ID, Author: e.PubKey, Prompt: e.Content, Root: root, Kind: e.Kind, CreatedAt: e.CreatedAt}, true
}

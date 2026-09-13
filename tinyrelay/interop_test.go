package tinyrelay_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/syncprotocol"
	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
	"github.com/FelineStateMachine/tinyrelay/tinyrelay"
	"github.com/coder/websocket"
)

type wireClient struct {
	t         *testing.T
	conn      *websocket.Conn
	challenge string
}

func openRelay(t *testing.T, authRequired bool) (*tinyrelay.Server, *httptest.Server, *wireClient) {
	t.Helper()
	ctx := context.Background()
	s, err := tinyrelay.OpenServer(ctx, tinyrelay.ServerConfig{
		DataDir:      t.TempDir(),
		PublicURL:    "ws://relay.example",
		AuthRequired: authRequired,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s)
	t.Cleanup(func() {
		httpServer.Close()
		if err := s.Close(); err != nil {
			t.Errorf("close relay: %v", err)
		}
	})
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &wireClient{t: t, conn: conn}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "test complete") })
	auth := client.expect("AUTH", 2)
	if err := json.Unmarshal(auth[1], &client.challenge); err != nil {
		t.Fatalf("decode relay challenge: %v", err)
	}
	return s, httpServer, client
}

func (c *wireClient) send(v ...any) {
	c.t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.conn.Write(context.Background(), websocket.MessageText, raw); err != nil {
		c.t.Fatal(err)
	}
}

func (c *wireClient) read() []json.RawMessage {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	typ, data, err := c.conn.Read(ctx)
	if err != nil {
		c.t.Fatalf("read relay message: %v", err)
	}
	if typ != websocket.MessageText {
		c.t.Fatalf("relay message type = %v, want text", typ)
	}
	var msg []json.RawMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		c.t.Fatalf("decode relay message %q: %v", data, err)
	}
	return msg
}

func (c *wireClient) expect(kind string, fields int) []json.RawMessage {
	c.t.Helper()
	msg := c.read()
	var got string
	if len(msg) == 0 || json.Unmarshal(msg[0], &got) != nil || got != kind {
		c.t.Fatalf("message kind = %q, want %q", got, kind)
	}
	if fields > 0 && len(msg) != fields {
		c.t.Fatalf("%s field count = %d, want %d", kind, len(msg), fields)
	}
	return msg
}

func signedEvent(t *testing.T, secret string, kind int, created int64, tags [][]string, content string) nostr.Event {
	t.Helper()
	e := nostr.Event{Kind: kind, CreatedAt: created, Tags: tags, Content: content}
	if err := nostr.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	return e
}

func publish(t *testing.T, c *wireClient, e nostr.Event) []json.RawMessage {
	t.Helper()
	c.send("EVENT", e)
	msg := c.expect("OK", 4)
	var id string
	if err := json.Unmarshal(msg[1], &id); err != nil || id != e.ID {
		t.Fatalf("OK id = %q, want %q", id, e.ID)
	}
	return msg
}

func assertAccepted(t *testing.T, msg []json.RawMessage, want bool) {
	t.Helper()
	var accepted bool
	if err := json.Unmarshal(msg[2], &accepted); err != nil {
		t.Fatalf("decode OK accepted flag: %v", err)
	}
	if accepted != want {
		t.Fatalf("OK accepted = %v, want %v", accepted, want)
	}
}

func TestStandaloneRelayNIP11AndNIP01Wire(t *testing.T) {
	_, httpServer, publisher := openRelay(t, false)
	req, err := http.NewRequest(http.MethodGet, httpServer.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/nostr+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/nostr+json") {
		t.Fatalf("NIP-11 content type = %q", got)
	}
	var info struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode NIP-11: %v", err)
	}
	if info.Name == "" {
		t.Fatal("NIP-11 has no relay name")
	}

	secret, err := nostr.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	e := signedEvent(t, secret, 1, now, nil, "first")
	publish(t, publisher, e)
	duplicate := publish(t, publisher, e)
	var reason string
	_ = json.Unmarshal(duplicate[3], &reason)
	if !strings.Contains(reason, "duplicate") {
		t.Fatalf("duplicate reason = %q", reason)
	}

	reader := func() *wireClient {
		conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		c := &wireClient{t: t, conn: conn}
		t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "test complete") })
		auth := c.expect("AUTH", 2)
		if err := json.Unmarshal(auth[1], &c.challenge); err != nil {
			t.Fatal(err)
		}
		return c
	}()
	reader.send("REQ", "notes", map[string]any{"ids": []string{e.ID}})
	got := reader.expect("EVENT", 3)
	var returned nostr.Event
	if err := json.Unmarshal(got[2], &returned); err != nil || returned.ID != e.ID {
		t.Fatalf("REQ returned wrong event: %v", err)
	}
	reader.expect("EOSE", 2)
	reader.send("CLOSE", "notes")
	reader.send("REQ", "live", map[string]any{"kinds": []int{1}})
	for {
		msg := reader.read()
		var kind string
		_ = json.Unmarshal(msg[0], &kind)
		if kind == "EOSE" {
			break
		}
		if kind != "EVENT" {
			t.Fatalf("live query response = %q", kind)
		}
	}
	// Generic Nostr reports are ordinary events for a standalone relay. They
	// must not trigger the daemon's application ACL subscription invalidation.
	report := signedEvent(t, secret, 1984, now+1, nil, "ordinary report")
	publish(t, publisher, report)
	live := signedEvent(t, secret, 1, now+1, nil, "live")
	publish(t, publisher, live)
	liveMessage := reader.expect("EVENT", 3)
	var liveEvent nostr.Event
	if err := json.Unmarshal(liveMessage[2], &liveEvent); err != nil || liveEvent.ID != live.ID {
		t.Fatalf("live event = %q, want %q", liveEvent.ID, live.ID)
	}
	reader.send("CLOSE", "live")

	invalid := e
	invalid.Content = "tampered"
	publisher.send("EVENT", invalid)
	assertAccepted(t, publisher.expect("OK", 4), false)
}

func TestStandaloneRelayEventLifecycleAndCount(t *testing.T) {
	_, _, c := openRelay(t, false)
	secret, err := nostr.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	first := signedEvent(t, secret, 0, now, nil, "old profile")
	publish(t, c, first)
	second := signedEvent(t, secret, 0, now+1, nil, "new profile")
	publish(t, c, second)
	deleted := signedEvent(t, secret, 5, now+2, [][]string{{"e", second.ID}}, "remove")
	publish(t, c, deleted)
	expired := signedEvent(t, secret, 1, now+3, [][]string{{"expiration", fmt.Sprint(now - 1)}}, "expired")
	publish(t, c, expired)
	ephemeral := signedEvent(t, secret, 20001, now+4, nil, "live only")
	publish(t, c, ephemeral)
	noteA := signedEvent(t, secret, 1, now+5, nil, "count A")
	publish(t, c, noteA)
	noteB := signedEvent(t, secret, 1, now+6, nil, "count B")
	publish(t, c, noteB)

	c.send("REQ", "state", map[string]any{"authors": []string{first.PubKey}, "kinds": []int{0, 1, 20001}})
	for {
		msg := c.read()
		var kind string
		_ = json.Unmarshal(msg[0], &kind)
		if kind == "EOSE" {
			break
		}
		if kind == "EVENT" {
			var got nostr.Event
			if err := json.Unmarshal(msg[2], &got); err != nil {
				t.Fatal(err)
			}
			if got.ID == first.ID || got.ID == second.ID || got.ID == expired.ID || got.ID == ephemeral.ID {
				t.Fatalf("lifecycle query returned excluded event %s", got.ID)
			}
		}
	}

	c.send("COUNT", "count", map[string]any{"kinds": []int{1}, "limit": 1}, map[string]any{"ids": []string{noteA.ID, noteB.ID}, "limit": 1}, map[string]any{"ids": []string{noteA.ID}, "limit": 1})
	count := c.expect("COUNT", 3)
	var result struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(count[2], &result); err != nil {
		t.Fatal(err)
	}
	if result.Count != 2 {
		t.Fatalf("COUNT = %d, want 2 across limited filters", result.Count)
	}
}

func TestStandaloneRelayReplaceableAddressableAndRestart(t *testing.T) {
	dataDir := t.TempDir()
	start := func() (*tinyrelay.Server, *httptest.Server, *wireClient) {
		s, err := tinyrelay.OpenServer(context.Background(), tinyrelay.ServerConfig{DataDir: dataDir, PublicURL: "ws://relay.example"})
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(s)
		conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(ts.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		c := &wireClient{t: t, conn: conn}
		auth := c.expect("AUTH", 2)
		if err := json.Unmarshal(auth[1], &c.challenge); err != nil {
			t.Fatal(err)
		}
		return s, ts, c
	}
	closeServer := func(s *tinyrelay.Server, ts *httptest.Server, c *wireClient) {
		_ = c.conn.Close(websocket.StatusNormalClosure, "restart")
		ts.Close()
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}

	s, ts, c := start()
	secret, err := nostr.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	profileOld := signedEvent(t, secret, 0, now, nil, "old")
	profileNew := signedEvent(t, secret, 0, now+1, nil, "new")
	addressOld := signedEvent(t, secret, 30000, now, [][]string{{"d", "profile"}}, "old address")
	addressNew := signedEvent(t, secret, 30000, now+1, [][]string{{"d", "profile"}}, "new address")
	for _, e := range []nostr.Event{profileOld, profileNew, addressOld, addressNew} {
		publish(t, c, e)
	}
	c.send("REQ", "current", map[string]any{"authors": []string{profileOld.PubKey}, "kinds": []int{0, 30000}})
	ids := map[string]bool{}
	for {
		msg := c.read()
		var kind string
		_ = json.Unmarshal(msg[0], &kind)
		if kind == "EOSE" {
			break
		}
		if kind != "EVENT" {
			t.Fatalf("replaceable query response = %q", kind)
		}
		var e nostr.Event
		if err := json.Unmarshal(msg[2], &e); err != nil {
			t.Fatal(err)
		}
		ids[e.ID] = true
	}
	if len(ids) != 2 || !ids[profileNew.ID] || !ids[addressNew.ID] || ids[profileOld.ID] || ids[addressOld.ID] {
		t.Fatalf("current replaceable events = %v", ids)
	}
	closeServer(s, ts, c)

	s, ts, c = start()
	t.Cleanup(func() { closeServer(s, ts, c) })
	c.send("REQ", "after-restart", map[string]any{"ids": []string{profileNew.ID, addressNew.ID}})
	ids = map[string]bool{}
	for {
		msg := c.read()
		var kind string
		_ = json.Unmarshal(msg[0], &kind)
		if kind == "EOSE" {
			break
		}
		if kind == "EVENT" {
			var e nostr.Event
			if err := json.Unmarshal(msg[2], &e); err != nil {
				t.Fatal(err)
			}
			ids[e.ID] = true
		}
	}
	if len(ids) != 2 {
		t.Fatalf("events after restart = %v", ids)
	}
}

func TestStandaloneRelayNIP42AndNIP77(t *testing.T) {
	_, _, c := openRelay(t, true)
	secret, err := nostr.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	c.send("REQ", "before-auth", map[string]any{})
	deniedRead := c.expect("CLOSED", 3)
	var deniedReason string
	_ = json.Unmarshal(deniedRead[2], &deniedReason)
	if !strings.Contains(deniedReason, "auth-required") {
		t.Fatalf("unauthenticated read reason = %q", deniedReason)
	}
	initiator, err := syncprotocol.NewSession(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	start, err := initiator.Start()
	if err != nil {
		t.Fatal(err)
	}
	c.send("NEG-OPEN", "before-auth-sync", map[string]any{}, start)
	negDenied := c.expect("NEG-ERR", 3)
	var negReason string
	_ = json.Unmarshal(negDenied[2], &negReason)
	if !strings.Contains(negReason, "auth-required") {
		t.Fatalf("unauthenticated sync reason = %q", negReason)
	}
	unauthenticated := signedEvent(t, secret, 1, time.Now().Unix(), nil, "before auth")
	c.send("EVENT", unauthenticated)
	assertAccepted(t, c.expect("OK", 4), false)
	protected := signedEvent(t, secret, 1, time.Now().Unix(), [][]string{{"-"}}, "protected")
	c.send("EVENT", protected)
	protectedReply := c.expect("OK", 4)
	assertAccepted(t, protectedReply, false)
	var protectedReason string
	_ = json.Unmarshal(protectedReply[3], &protectedReason)
	if !strings.Contains(protectedReason, "auth-required") {
		t.Fatalf("unauthenticated protected publish reason = %q", protectedReason)
	}
	auth := signedEvent(t, secret, 22242, time.Now().Unix(), [][]string{{"relay", "ws://relay.example"}, {"challenge", c.challenge}}, "")
	c.send("AUTH", auth)
	authReply := c.expect("OK", 4)
	var accepted bool
	if err := json.Unmarshal(authReply[2], &accepted); err != nil || !accepted {
		t.Fatalf("AUTH rejected: %s", authReply[3])
	}
	publish(t, c, unauthenticated)
	publish(t, c, protected)
	otherSecret, err := nostr.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	other := signedEvent(t, otherSecret, 1, time.Now().Unix(), nil, "other author")
	c.send("EVENT", other)
	assertAccepted(t, c.expect("OK", 4), false)
	otherProtected := signedEvent(t, otherSecret, 1, time.Now().Unix(), [][]string{{"-"}}, "other protected")
	c.send("EVENT", otherProtected)
	assertAccepted(t, c.expect("OK", 4), false)
	// An empty client snapshot reconciles with the relay and exercises the
	// externally visible NEG-OPEN/NEG-CLOSE exchange without private storage APIs.
	initiator, err = syncprotocol.NewSession(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	start, err = initiator.Start()
	if err != nil {
		t.Fatal(err)
	}
	c.send("NEG-OPEN", "sync", map[string]any{}, start)
	msg := c.read()
	var kind string
	_ = json.Unmarshal(msg[0], &kind)
	if kind != "NEG-CLOSE" && kind != "NEG-MSG" {
		t.Fatalf("NEG-OPEN response kind = %q", kind)
	}
}

func TestStandaloneRelayCountAuthRequiredWire(t *testing.T) {
	_, httpServer, anonymous := openRelay(t, true)
	anonymous.send("COUNT", "anonymous-count", map[string]any{})
	denied := anonymous.expect("CLOSED", 3)
	var reason string
	if err := json.Unmarshal(denied[2], &reason); err != nil {
		t.Fatalf("decode COUNT denial reason: %v", err)
	}
	if reason != "auth-required: this relay requires AUTH" {
		t.Fatalf("anonymous COUNT reason = %q", reason)
	}

	secret, err := nostr.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	authenticated := connectAuthClient(t, httpServer.URL)
	authenticated.send("AUTH", signedEvent(t, secret, 22242, time.Now().Unix(), [][]string{{"relay", "ws://relay.example"}, {"challenge", authenticated.challenge}}, ""))
	assertAccepted(t, authenticated.expect("OK", 4), true)
	authenticated.send("COUNT", "authenticated-count", map[string]any{})
	count := authenticated.expect("COUNT", 3)
	var value struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(count[2], &value); err != nil {
		t.Fatalf("decode authenticated COUNT: %v", err)
	}
	if value.Count != 0 {
		t.Fatalf("authenticated COUNT = %d, want 0", value.Count)
	}
}

func TestStandaloneRelayGenericInboxAndRegistrationKinds(t *testing.T) {
	_, httpServer, publisher := openRelay(t, false)
	recipientSecret, err := nostr.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := nostr.PublicKey(recipientSecret)
	if err != nil {
		t.Fatal(err)
	}
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	reader := &wireClient{t: t, conn: conn}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "test complete") })
	auth := reader.expect("AUTH", 2)
	if err := json.Unmarshal(auth[1], &reader.challenge); err != nil {
		t.Fatal(err)
	}
	reader.send("REQ", "inbox", map[string]any{"kinds": []int{24133}, "#p": []string{recipient}})
	reader.expect("EOSE", 2)
	secret, err := nostr.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	inbox := signedEvent(t, secret, 24133, time.Now().Unix(), [][]string{{"p", recipient}}, "encrypted reply")
	publish(t, publisher, inbox)
	live := reader.expect("EVENT", 3)
	var liveEvent nostr.Event
	if err := json.Unmarshal(live[2], &liveEvent); err != nil || liveEvent.ID != inbox.ID {
		t.Fatalf("live inbox event = %q, want %q", liveEvent.ID, inbox.ID)
	}
	registration := signedEvent(t, secret, 30390, time.Now().Unix()+1, [][]string{{"d", "device"}}, "registration")
	publish(t, publisher, registration)
	reader.send("REQ", "registration", map[string]any{"kinds": []int{30390}, "#d": []string{"device"}})
	got := reader.expect("EVENT", 3)
	var stored nostr.Event
	if err := json.Unmarshal(got[2], &stored); err != nil || stored.ID != registration.ID {
		t.Fatalf("registration event = %q, want %q", stored.ID, registration.ID)
	}
	reader.expect("EOSE", 2)
}

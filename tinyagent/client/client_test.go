package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
	"github.com/coder/websocket"
)

func TestRequestURLRejectsExternalAndEscapedPaths(t *testing.T) {
	c := Client{BaseURL: "https://relay.example"}
	for _, path := range []string{"https://evil.example/x", "//evil.example/x", "/../secret", "/rooms/%2e%2e/secret", "rooms/x"} {
		if _, err := c.requestURL(path); err == nil {
			t.Errorf("requestURL(%q) accepted unsafe path", path)
		}
	}
	got, err := c.requestURL("/healthz?ok=1")
	if err != nil || got.String() != "https://relay.example/healthz?ok=1" {
		t.Fatalf("safe path: %v %v", got, err)
	}
}

func TestWebsocketURL(t *testing.T) {
	for raw, want := range map[string]string{"https://relay.example/x": "wss://relay.example/x", "http://relay.example": "ws://relay.example"} {
		u, err := websocketURL(raw)
		if err != nil || u != want {
			t.Errorf("websocketURL(%q) = %q, %v; want %q", raw, u, err, want)
		}
	}
	if _, err := websocketURL("file:///tmp/x"); err == nil {
		t.Error("accepted invalid websocket URL")
	}
	_ = url.URL{}
}

func TestPublishChecksAcceptedReceiptAndUsesFreshNIP98Nonce(t *testing.T) {
	secret, err := nostr.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	c, err := New("http://relay.example/t/lab", secret)
	if err != nil {
		t.Fatal(err)
	}
	var proofs []string
	c.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		proofs = append(proofs, req.Header.Get("Authorization"))
		if req.URL.Path != "/t/lab/events" {
			t.Fatalf("path = %s", req.URL.Path)
		}
		return response(`{"accepted":false,"message":"blocked: test"}`), nil
	})}
	e := nostr.Event{Kind: 9, Content: "test"}
	if _, err := c.Publish(context.Background(), e); err == nil || !strings.Contains(err.Error(), "blocked: test") {
		t.Fatalf("publish error = %v", err)
	}
	if len(proofs) != 1 || !strings.Contains(proofs[0], "Nostr ") {
		t.Fatalf("proof = %#v", proofs)
	}
	first, err := c.proof(http.MethodGet, "http://relay.example/t/lab/query", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.proof(http.MethodGet, "http://relay.example/t/lab/query", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("identical NIP98 requests reused a proof")
	}
}

func TestPublishRejectsMalformedReceipt(t *testing.T) {
	secret, err := nostr.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	c, err := New("http://relay.example", secret)
	if err != nil {
		t.Fatal(err)
	}
	c.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response("not-json"), nil
	})}
	if _, err := c.Publish(context.Background(), nostr.Event{Kind: 9}); err == nil || !strings.Contains(err.Error(), "invalid publish receipt") {
		t.Fatalf("publish error = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func response(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestQueryAuthenticatesAndFiltersSocketFrames(t *testing.T) {
	clientSecret, err := nostr.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	eventSecret, err := nostr.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var req []json.RawMessage
		if json.Unmarshal(raw, &req) != nil {
			return
		}
		_ = writeFrame(ctx, conn, []any{"AUTH", "challenge"})
		_, raw, err = conn.Read(ctx)
		if err != nil {
			return
		}
		var auth []json.RawMessage
		if json.Unmarshal(raw, &auth) != nil || len(auth) < 2 {
			return
		}
		var proof nostr.Event
		if json.Unmarshal(auth[1], &proof) != nil {
			return
		}
		_ = writeFrame(ctx, conn, []any{"OK", proof.ID, true, ""})
		_, _, err = conn.Read(ctx)
		if err != nil {
			return
		}
		matching := nostr.Event{Kind: 9, CreatedAt: 2, Tags: [][]string{{"h", "room"}}, Content: "match"}
		if err := nostr.Sign(&matching, eventSecret); err != nil {
			return
		}
		wrongRoom := matching
		wrongRoom.Tags = [][]string{{"h", "other"}}
		if err := nostr.Sign(&wrongRoom, eventSecret); err != nil {
			return
		}
		invalid := matching
		invalid.Content = "tampered"
		_ = writeFrame(ctx, conn, []any{"EVENT", "wrong-sub", matching})
		_ = writeFrame(ctx, conn, []any{"EVENT", "tinyagent-query", invalid})
		_ = writeFrame(ctx, conn, []any{"EVENT", "tinyagent-query", wrongRoom})
		_ = writeFrame(ctx, conn, []any{"EVENT", "tinyagent-query", matching})
		_ = writeFrame(ctx, conn, []any{"EOSE", "tinyagent-query"})
	}))
	defer server.Close()
	c, err := New(server.URL, clientSecret)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Query(context.Background(), []Filter{{"#h": []any{"room"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "match" {
		t.Fatalf("query returned %#v", got)
	}
}

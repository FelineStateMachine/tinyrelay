package daemon

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/coder/websocket"
)

func TestNostrConnectRepliesArriveBeforeAuthentication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	app, tenant := testTenant(t)
	policy := tenant.Policy()
	policy.Reads, policy.Writes = "members", "owner"
	policy.Features.Signer = true
	if err := tenant.applyPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app)
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	subscriber, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer subscriber.CloseNow()
	publisher, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.CloseNow()
	readRelayMessage(t, ctx, subscriber) // AUTH challenge; the signer is not connected yet.
	readRelayMessage(t, ctx, publisher)
	recipient := strings.Repeat("b", 64)
	if err := writeRelayMessage(ctx, subscriber, []any{"REQ", "signer", map[string]any{"kinds": []int{24133}, "#p": []string{recipient}, "limit": 0}}); err != nil {
		t.Fatal(err)
	}
	if data := readRelayMessage(t, ctx, subscriber); !strings.Contains(string(data), `"EOSE","signer"`) {
		t.Fatalf("subscription rejected: %s", data)
	}
	message := event.Event{Kind: 24133, CreatedAt: time.Now().Unix(), Tags: [][]string{{"p", recipient}}, Content: "encrypted signer reply"}
	if err := event.Sign(&message, strings.Repeat("0", 63)+"2"); err != nil {
		t.Fatal(err)
	}
	if err := writeRelayMessage(ctx, publisher, []any{"EVENT", message}); err != nil {
		t.Fatal(err)
	}
	if data := readRelayMessage(t, ctx, publisher); !strings.Contains(string(data), `"`+message.ID+`",true`) {
		t.Fatalf("signer reply rejected: %s", data)
	}
	if data := readRelayMessage(t, ctx, subscriber); !strings.Contains(string(data), `"EVENT","signer"`) || !strings.Contains(string(data), message.ID) {
		t.Fatalf("signer reply not delivered: %s", data)
	}
}

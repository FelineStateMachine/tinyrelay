package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/coder/websocket"
)

func TestNIP43InviteMountedRequestIsRelaySignedAndScoped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	app, tenant := testTenant(t)
	secret := strings.Repeat("0", 63) + "1"
	server := httptest.NewServer(app)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/r/main"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.CloseNow()
		server.Close()
	})

	_, challengeRaw, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var challengeMessage []json.RawMessage
	if err := json.Unmarshal(challengeRaw, &challengeMessage); err != nil || len(challengeMessage) < 2 {
		t.Fatalf("AUTH challenge: %s", challengeRaw)
	}
	var challenge string
	if err := json.Unmarshal(challengeMessage[1], &challenge); err != nil {
		t.Fatal(err)
	}
	auth := event.Event{Kind: event.KIND_AUTH, CreatedAt: time.Now().Unix(), Tags: [][]string{{"relay", wsURL}, {"challenge", challenge}}}
	if err := event.Sign(&auth, secret); err != nil {
		t.Fatal(err)
	}
	if err := writeRelayMessage(ctx, conn, []any{"AUTH", auth}); err != nil {
		t.Fatal(err)
	}
	if data := readRelayMessage(t, ctx, conn); !strings.Contains(string(data), `"OK","`+auth.ID+`",true`) {
		t.Fatalf("AUTH rejected: %s", data)
	}

	if err := writeRelayMessage(ctx, conn, []any{"REQ", "zero", map[string]any{"kinds": []int{event.KIND_NIP43_INVITE}, "limit": 0}}); err != nil {
		t.Fatal(err)
	}
	if data := readRelayMessage(t, ctx, conn); !strings.Contains(string(data), `"EOSE","zero"`) {
		t.Fatalf("limit zero response: %s", data)
	}
	var claims int
	if err := tenant.store.DB().QueryRow(`SELECT count(*) FROM community_invites`).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if claims != 0 {
		t.Fatalf("limit zero minted %d claims", claims)
	}
	now := time.Now().Unix()
	impossible := []struct {
		name   string
		filter map[string]any
	}{
		{name: "wrong id", filter: map[string]any{"kinds": []int{event.KIND_NIP43_INVITE}, "ids": []string{strings.Repeat("f", 64)}}},
		{name: "wrong author", filter: map[string]any{"kinds": []int{event.KIND_NIP43_INVITE}, "authors": []string{strings.Repeat("e", 64)}}},
		{name: "wrong tag", filter: map[string]any{"kinds": []int{event.KIND_NIP43_INVITE}, "#p": []string{strings.Repeat("d", 64)}}},
		{name: "expired", filter: map[string]any{"kinds": []int{event.KIND_NIP43_INVITE}, "until": now - 60}},
		{name: "future", filter: map[string]any{"kinds": []int{event.KIND_NIP43_INVITE}, "since": now + 60}},
	}
	for _, tc := range impossible {
		if err := writeRelayMessage(ctx, conn, []any{"REQ", tc.name, tc.filter}); err != nil {
			t.Fatal(err)
		}
		if data := readRelayMessage(t, ctx, conn); !strings.Contains(string(data), `"EOSE","`+tc.name+`"`) {
			t.Fatalf("%s response: %s", tc.name, data)
		}
	}
	if err := tenant.store.DB().QueryRow(`SELECT count(*) FROM community_invites`).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if claims != 0 {
		t.Fatalf("impossible filters minted %d claims", claims)
	}

	if err := writeRelayMessage(ctx, conn, []any{"REQ", "mixed", map[string]any{"kinds": []int{event.KIND_NIP43_INVITE, 1}, "limit": 1}}); err != nil {
		t.Fatal(err)
	}
	data := readRelayMessage(t, ctx, conn)
	var message []json.RawMessage
	if err := json.Unmarshal(data, &message); err != nil || len(message) < 3 {
		t.Fatalf("invite response: %s", data)
	}
	var responseKind string
	if err := json.Unmarshal(message[0], &responseKind); err != nil || responseKind != "EVENT" {
		t.Fatalf("expected EVENT, got %s", data)
	}
	var invite event.Event
	if err := json.Unmarshal(message[2], &invite); err != nil {
		t.Fatal(err)
	}
	if invite.Kind != event.KIND_NIP43_INVITE || invite.PubKey != tenant.records.PublicKey() {
		t.Fatalf("unexpected invite identity: %+v", invite)
	}
	if err := event.Validate(invite); err != nil {
		t.Fatalf("relay signature invalid: %v", err)
	}
	if event.Tag(invite, "claim") == "" {
		t.Fatal("invite omitted claim")
	}
}

func writeRelayMessage(ctx context.Context, conn *websocket.Conn, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

func readRelayMessage(t *testing.T, ctx context.Context, conn *websocket.Conn) []byte {
	t.Helper()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

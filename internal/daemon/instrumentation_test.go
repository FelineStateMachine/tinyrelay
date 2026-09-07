package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/telemetry"
)

func TestInstrumentRelayConfigRecordsConnectionLifecycle(t *testing.T) {
	metrics, err := telemetry.New(context.Background(), telemetry.Config{DebugToken: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer metrics.Close(context.Background())
	cfg := InstrumentRelayConfig(relay.Config{}, metrics)
	if cfg.OnConnection == nil || cfg.OnSubscription == nil {
		t.Fatal("relay callbacks were not attached")
	}
	cfg.OnConnection(1)
	cfg.OnSubscription(1)
	cfg.OnSubscription(-1)
	cfg.OnConnection(-1)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, req)
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "relay_connections 0") {
		t.Fatalf("connection gauge missing: %s", text)
	}
	if !strings.Contains(text, "relay_subscriptions 0") {
		t.Fatalf("subscription gauge missing: %s", text)
	}
}

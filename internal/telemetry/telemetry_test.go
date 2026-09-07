package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTelemetryMetricsArePrivateAndBounded(t *testing.T) {
	tel, err := New(context.Background(), Config{Version: "test", DebugToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTelemetry(t, tel)

	ctx, done := tel.Start(context.Background(), "publish")
	if ctx == nil {
		t.Fatal("Start returned nil context")
	}
	done("success")
	tel.ObserveStorage("query", "success", 2*time.Millisecond)
	tel.Connection(1)
	tel.Subscription(1)
	tel.Delivery("success", time.Millisecond)
	tel.Queue("outbox", 3)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	tel.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, forbidden := range []string{"secret", "tenant-123", "npub1rawvalue"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("metrics leaked unbounded value %q", forbidden)
		}
	}
	if !strings.Contains(body, "relay_connections") || !strings.Contains(body, "relay_message_duration_seconds") {
		t.Fatal("expected relay metrics")
	}
}

func TestDiagnosticsRequireToken(t *testing.T) {
	tel, err := New(context.Background(), Config{DebugToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTelemetry(t, tel)

	for _, token := range []string{"", "wrong"} {
		req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rr := httptest.NewRecorder()
		tel.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("token %q status = %d", token, rr.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	tel.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid token status = %d", rr.Code)
	}
}

func TestHTTPOperationUsesHTTPMetricTransport(t *testing.T) {
	tel, err := New(context.Background(), Config{DebugToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTelemetry(t, tel)
	_, done := tel.Start(context.Background(), "http")
	done("ok")
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	tel.Handler().ServeHTTP(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, `relay_messages_received_total{outcome="ok",transport="http"} 1`) {
		t.Fatalf("missing HTTP transport metric: %s", body)
	}
}

func TestOTLPExportFlushesOnClose(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	tel, err := New(context.Background(), Config{OTLPEndpoint: server.URL, TraceSampleRatio: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, done := tel.Start(context.Background(), "publish")
	done("success")
	if err := tel.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() == 0 {
		t.Fatal("expected OTLP export request during shutdown")
	}
}

func closeTelemetry(t *testing.T, tel *Telemetry) {
	t.Helper()
	if err := tel.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

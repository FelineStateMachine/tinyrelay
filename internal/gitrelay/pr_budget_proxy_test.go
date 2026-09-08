package gitrelay

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func repairProxyClient(t *testing.T, ctx context.Context, target string, budget *byteBudget, limit int64) (*repairProxy, *http.Client) {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := newRepairProxy(ctx, u.Hostname(), u.Hostname(), u.Port(), budget, limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	proxyURL, _ := url.Parse(proxy.URL())
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // Local test server's self-signed certificate.
	t.Cleanup(transport.CloseIdleConnections)
	return proxy, &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func TestRepairProxyEnforcesSharedResponseBudget(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/small" {
			_, _ = io.WriteString(w, "first")
			return
		}
		_, _ = io.WriteString(w, strings.Repeat("x", 8192))
	}))
	defer server.Close()
	budget := &byteBudget{}
	_, client := repairProxyClient(t, context.Background(), server.URL, budget, 2048)
	response, err := client.Get(server.URL + "/small")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "first" {
		t.Fatalf("first response = %q, %v", body, err)
	}
	before := budget.used.Load()
	if before == 0 || before >= 2048 {
		t.Fatalf("first request consumed %d bytes", before)
	}
	response, err = client.Get(server.URL + "/large")
	if err == nil {
		body, err = io.ReadAll(response.Body)
		response.Body.Close()
		if len(body) >= 2048 {
			t.Fatalf("oversized response leaked %d bytes", len(body))
		}
	}
	if err == nil {
		t.Fatal("oversized response completed")
	}
	if budget.used.Load() != 2048 {
		t.Fatalf("budget = %d, want 2048", budget.used.Load())
	}
	if requests.Load() != 2 {
		t.Fatalf("upstream requests = %d, want two", requests.Load())
	}
	if response, err = client.Get(server.URL + "/small"); err == nil {
		response.Body.Close()
		t.Fatal("request passed after shared budget exhausted")
	}
	if requests.Load() != 2 {
		t.Fatal("exhausted proxy contacted upstream")
	}
}

func TestRepairProxyLimitsHTTPSTunnel(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 256<<10))
	}))
	defer server.Close()
	budget := &byteBudget{}
	_, client := repairProxyClient(t, context.Background(), server.URL, budget, 64<<10)
	response, err := client.Get(server.URL)
	if err == nil {
		body, bodyErr := io.ReadAll(response.Body)
		response.Body.Close()
		if len(body) >= 64<<10 {
			t.Fatalf("HTTPS body exceeded wire budget: %d", len(body))
		}
		err = bodyErr
	}
	if err == nil {
		t.Fatal("oversized HTTPS body completed")
	}
	if budget.used.Load() != 64<<10 {
		t.Fatalf("HTTPS budget = %d", budget.used.Load())
	}
}

func TestRepairProxyCancellationInterruptsStalledUpstream(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, client := repairProxyClient(t, ctx, server.URL, &byteBudget{}, 4096)
	done := make(chan error, 1)
	go func() {
		response, err := client.Get(server.URL)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not receive request")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled request succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not interrupt stalled request")
	}
}

func TestRepairProxyRejectsAnotherPort(t *testing.T) {
	var contacted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { contacted.Store(true) }))
	defer server.Close()
	_, client := repairProxyClient(t, context.Background(), "http://127.0.0.1:1", &byteBudget{}, 4096)
	if response, err := client.Get(server.URL); err == nil {
		response.Body.Close()
		t.Fatal("proxy accepted a different port")
	}
	if contacted.Load() {
		t.Fatal("proxy contacted a different target")
	}
}

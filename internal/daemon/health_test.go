package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProcessHealthAndReadinessLifecycle(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: t.TempDir(), DefaultTenant: "missing"})
	if err != nil {
		t.Fatal(err)
	}
	ready := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://relay.test/readyz", nil)
		res := httptest.NewRecorder()
		a.ServeHTTP(res, req)
		return res
	}
	if got := ready().Code; got != http.StatusServiceUnavailable {
		t.Fatalf("before start readiness = %d", got)
	}
	if err := a.Start(context.Background(), "http://relay.test"); err != nil {
		t.Fatal(err)
	}
	if got := ready().Code; got != http.StatusOK {
		t.Fatalf("after start readiness = %d", got)
	}
	healthReq := httptest.NewRequest(http.MethodGet, "http://relay.test/healthz", nil)
	healthRes := httptest.NewRecorder()
	a.ServeHTTP(healthRes, healthReq)
	if healthRes.Code != http.StatusOK {
		t.Fatalf("liveness = %d", healthRes.Code)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := ready().Code; got != http.StatusServiceUnavailable {
		t.Fatalf("after close readiness = %d", got)
	}
}

func TestProcessReadinessReportsCatalogFailure(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background(), "http://relay.test"); err != nil {
		t.Fatal(err)
	}
	if err := a.catalog.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://relay.test/readyz", nil)
	res := httptest.NewRecorder()
	a.ServeHTTP(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed catalog readiness = %d", res.Code)
	}
	_ = a.Close(context.Background())
}

func TestTenantHealthObeysMaintenanceAndStorage(t *testing.T) {
	a, tenant := testTenant(t)
	request := func(path string) int {
		req := httptest.NewRequest(http.MethodGet, "http://relay.test"+path, nil)
		res := httptest.NewRecorder()
		a.ServeHTTP(res, req)
		return res.Code
	}
	if got := request("/r/main/healthz"); got != http.StatusOK {
		t.Fatalf("tenant health = %d", got)
	}
	_, release, err := tenant.beginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := request("/r/main/readyz"); got != http.StatusServiceUnavailable {
		t.Fatalf("tenant readiness during maintenance = %d", got)
	}
	release()
	if got := request("/r/main/readyz"); got != http.StatusOK {
		t.Fatalf("tenant readiness after maintenance = %d", got)
	}
	// Stop the tenant workers before closing their shared store. Closing the
	// database underneath a live queue claim races cleanup and can turn this
	// health probe into an unrelated "database is closed" failure.
	if err := tenant.closeServices(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tenant.store.Close(); err != nil {
		t.Fatal(err)
	}
	if got := request("/r/main/healthz"); got != http.StatusServiceUnavailable {
		t.Fatalf("tenant health after storage close = %d", got)
	}
}

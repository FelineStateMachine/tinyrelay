package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnclaimedSubdomainsOfTheSiteDomainAreNotFound(t *testing.T) {
	ctx := context.Background()
	app, err := New(ctx, Config{DataDir: t.TempDir(), DefaultTenant: "main", PublicURL: "https://relay.test"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	meta, err := app.Create(ctx, CreateOptions{Name: "main", Owner: strings.Repeat("a", 64), Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := app.tenant(ctx, meta, "https://relay.test")
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]int{"relay.test": http.StatusOK, "bauhaus.relay.test": http.StatusNotFound, "tiny.tailnet.example": http.StatusOK, "127.0.0.1:7447": http.StatusOK} {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		req.Host = host
		res := httptest.NewRecorder()
		tenant.ServeHTTP(res, req)
		if res.Code != want {
			t.Errorf("host %s: status %d, want %d", host, res.Code, want)
		}
	}
}

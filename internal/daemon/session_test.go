package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func testTenant(t *testing.T) (*App, *Tenant) {
	t.Helper()
	ctx := context.Background()
	a, err := New(ctx, Config{DataDir: t.TempDir(), DefaultTenant: "main"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	pk, err := event.PublicKey(strings.Repeat("0", 63) + "1")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := a.Create(ctx, CreateOptions{Name: "main", Owner: pk, Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := a.tenant(ctx, meta, "http://relay.test")
	if err != nil {
		t.Fatal(err)
	}
	return a, tenant
}

func TestSignedSessionSurvivesNavigationAndRevokes(t *testing.T) {
	a, tenant := testTenant(t)
	req := httptest.NewRequest("POST", "http://relay.test/session", strings.NewReader(""))
	signRequest(t, req, "")
	res := httptest.NewRecorder()
	a.ServeHTTP(res, req)
	if res.Code != 200 {
		t.Fatalf("login %d: %s", res.Code, res.Body.String())
	}
	cookies := res.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie %+v", cookies)
	}
	page := httptest.NewRequest("GET", "http://relay.test/manage", nil)
	page.AddCookie(cookies[0])
	actor, err := tenant.resolveUIActor(page)
	if err != nil || actor != tenant.Policy().Owner {
		t.Fatalf("session actor %s %v", actor, err)
	}
	logout := httptest.NewRequest("POST", "http://relay.test/session/logout", nil)
	logout.AddCookie(cookies[0])
	logout.Header.Set("Origin", "https://foreign.test")
	denied := httptest.NewRecorder()
	a.ServeHTTP(denied, logout)
	if denied.Code != 403 {
		t.Fatal("foreign logout accepted")
	}
	logout.Header.Set("Origin", "http://relay.test")
	ok := httptest.NewRecorder()
	a.ServeHTTP(ok, logout)
	if ok.Code != 200 {
		t.Fatalf("logout %d", ok.Code)
	}
	actor, err = tenant.resolveUIActor(page)
	if err != nil || actor != "" {
		t.Fatalf("revoked session %s %v", actor, err)
	}
}

func TestTenantMaintenanceUsesPersistedRetention(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	now := time.Now().Unix()
	e := event.Event{Kind: 1, CreatedAt: now - 3*86400, Tags: [][]string{}, Content: "aged note"}
	if err := event.Sign(&e, strings.Repeat("0", 63)+"2"); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.Save(ctx, e, storage.SaveOptions{Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.DB().ExecContext(ctx, "INSERT OR REPLACE INTO community_retention(kind,days) VALUES(1,1)"); err != nil {
		t.Fatal(err)
	}
	if err := tenant.sweep(ctx, now); err != nil {
		t.Fatal(err)
	}
	rows, err := tenant.store.Query(ctx, event.Filter{IDs: []string{e.ID}}, storage.QueryOptions{Now: now})
	if err != nil || len(rows.Events) != 0 {
		t.Fatalf("retention did not remove old event: %+v %v", rows, err)
	}
}

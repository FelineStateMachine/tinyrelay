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
	// Reads through the query bridge use the session when the request comes
	// from this origin; foreign origins and publishing still need a signature.
	query := func(path, origin string) int {
		req := httptest.NewRequest("POST", "http://relay.test"+path, strings.NewReader(`[{"kinds":[10318],"limit":1}]`))
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		req.AddCookie(cookies[0])
		res := httptest.NewRecorder()
		a.ServeHTTP(res, req)
		return res.Code
	}
	if code := query("/query", "http://relay.test"); code != 200 {
		t.Fatalf("session query %d", code)
	}
	if code := query("/query", "https://foreign.test"); code != 401 {
		t.Fatalf("foreign origin query %d", code)
	}
	if code := query("/query", ""); code != 401 {
		t.Fatalf("query without origin %d", code)
	}
	if code := query("/events", "http://relay.test"); code != 401 {
		t.Fatalf("cookie publish %d", code)
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

func TestChatPreferencesArePrivateAndDurablePerAccount(t *testing.T) {
	a, tenant := testTenant(t)
	login := httptest.NewRequest(http.MethodPost, "http://relay.test/session", strings.NewReader(""))
	signRequest(t, login, "")
	loginResult := httptest.NewRecorder()
	a.ServeHTTP(loginResult, login)
	if loginResult.Code != http.StatusOK {
		t.Fatalf("login: %d %s", loginResult.Code, loginResult.Body.String())
	}
	cookie := loginResult.Result().Cookies()[0]
	read := httptest.NewRequest(http.MethodGet, "http://relay.test/chat/preferences", nil)
	read.AddCookie(cookie)
	result := httptest.NewRecorder()
	a.ServeHTTP(result, read)
	if result.Code != http.StatusOK || result.Body.String() != `{"share_presence":false}` {
		t.Fatalf("default preference: %d %s", result.Code, result.Body.String())
	}
	update := httptest.NewRequest(http.MethodPost, "http://relay.test/chat/preferences", strings.NewReader(`{"share_presence":true}`))
	signRequest(t, update, `{"share_presence":true}`)
	updated := httptest.NewRecorder()
	a.ServeHTTP(updated, update)
	if updated.Code != http.StatusOK || updated.Body.String() != `{"share_presence":true}` {
		t.Fatalf("updated preference: %d %s", updated.Code, updated.Body.String())
	}
	if !tenant.chatSharePresence(context.Background(), tenant.Policy().Owner) {
		t.Fatal("preference was not persisted")
	}
	reloaded := httptest.NewRequest(http.MethodGet, "http://relay.test/chat/preferences", nil)
	reloaded.AddCookie(cookie)
	reloadedResult := httptest.NewRecorder()
	a.ServeHTTP(reloadedResult, reloaded)
	if reloadedResult.Code != http.StatusOK || reloadedResult.Body.String() != `{"share_presence":true}` {
		t.Fatalf("reloaded preference: %d %s", reloadedResult.Code, reloadedResult.Body.String())
	}
	guest := httptest.NewRecorder()
	a.ServeHTTP(guest, httptest.NewRequest(http.MethodGet, "http://relay.test/chat/preferences", nil))
	if guest.Code != http.StatusUnauthorized {
		t.Fatalf("guest preference: %d", guest.Code)
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

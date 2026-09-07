package domains

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/catalog"
)

const domainOwner = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testService(t *testing.T) (*Service, *catalog.Catalog, string) {
	t.Helper()
	cat, err := catalog.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	tenant, err := cat.Create(context.Background(), catalog.CreateOptions{Name: "relay", Owner: domainOwner})
	if err != nil {
		t.Fatal(err)
	}
	returnMust := func() *Service {
		s, newErr := New(Config{Catalog: cat, TenantID: tenant.ID, BaseHost: "tiny.example", RelayURL: "https://tiny.example/relay", Owner: func(actor string) bool { return actor == domainOwner }, ResolveDNS: func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("203.0.113.7")}, nil }})
		if newErr != nil {
			t.Fatal(newErr)
		}
		return s
	}
	return returnMust(), cat, tenant.ID
}

func TestDomainManagementUsesExplicitLocalMapping(t *testing.T) {
	s, cat, tenantID := testService(t)
	ctx := context.Background()
	view, err := s.Execute(ctx, domainOwner, "adddomain", raws(`"relay.alice.example"`, `null`))
	if err != nil {
		t.Fatal(err)
	}
	if view.(View).Status != "configured" || !view.(View).Ready {
		t.Fatalf("add view = %+v", view)
	}
	resolved, err := cat.ResolveHost(ctx, "relay.alice.example")
	if err != nil || resolved.ID != tenantID {
		t.Fatalf("resolved = %+v, err=%v", resolved, err)
	}
	checked, err := s.Execute(ctx, domainOwner, "checkdomain", raws(`"relay.alice.example"`))
	if err != nil || len(checked.(View).Addresses) != 1 {
		t.Fatalf("check = %+v, err=%v", checked, err)
	}
	if _, err := s.Execute(ctx, "not-owner", "adddomain", raws(`"other.example"`)); err == nil {
		t.Fatal("non-owner domain mutation succeeded")
	}
	if _, err := s.Execute(ctx, domainOwner, "adddomain", raws(`"*.example"`)); err == nil {
		t.Fatal("wildcard accepted")
	}
	if _, err := s.Execute(ctx, domainOwner, "adddomain", raws(`"relay.tiny.example"`)); err == nil {
		t.Fatal("base-host child accepted")
	}
	if err := s.Remove(ctx, "relay.alice.example"); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ResolveHostMapping(ctx, "relay.alice.example"); !errors.Is(err, catalog.ErrHostMappingNotFound) {
		t.Fatalf("removed mapping err=%v", err)
	}
}

func TestWebAddressDiscoveryValidationAndResponse(t *testing.T) {
	h := WebAddressHandler(AddressConfig{RelayURL: "wss://relay.example", Authorized: func(*http.Request) bool { return true }, Lookup: func(context.Context, string) (map[string]any, bool, error) {
		return map[string]any{"ids": []string{"abc"}}, true, nil
	}})
	req := httptest.NewRequest(http.MethodGet, "https://relay.example/.well-known/nostr.json?path=%2Fe%2Fabc", nil)
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"/e/abc"`) || !strings.Contains(resp.Body.String(), `wss://relay.example`) {
		t.Fatalf("response %d %s", resp.Code, resp.Body.String())
	}
	bad := httptest.NewRecorder()
	h.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "https://relay.example/.well-known/nostr.json?path=%2F..%2Fx", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad path status = %d", bad.Code)
	}
	unauth := WebAddressHandler(AddressConfig{Authorized: func(*http.Request) bool { return false }})
	got := httptest.NewRecorder()
	unauth.ServeHTTP(got, httptest.NewRequest(http.MethodGet, "https://relay.example/.well-known/nostr.json?path=%2F", nil))
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", got.Code)
	}
}

func raws(values ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(values))
	for i, value := range values {
		out[i] = json.RawMessage(value)
	}
	return out
}

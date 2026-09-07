package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebMCPReadsUseBrowserSessionPermissions(t *testing.T) {
	app, tenant := testTenant(t)
	policy := tenant.Policy()
	policy.Features.Grasp = true
	if err := tenant.applyPolicy(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPost, "http://relay.test/session", strings.NewReader(""))
	signRequest(t, login, "")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, login)
	if response.Code != http.StatusOK || len(response.Result().Cookies()) != 1 {
		t.Fatalf("sign in: status %d", response.Code)
	}
	cookie := response.Result().Cookies()[0]
	for _, tc := range []struct {
		name, method string
		signedIn     bool
		status       int
	}{
		{"public repositories", "browserepos", false, http.StatusOK},
		{"protected status", "browsestatus", false, http.StatusForbidden},
		{"owner status", "browsestatus", true, http.StatusOK},
		{"mutation rejected", "setpolicy", true, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://relay.test/webmcp/query?method="+tc.method, nil)
			if tc.signedIn {
				request.AddCookie(cookie)
			}
			result := httptest.NewRecorder()
			app.ServeHTTP(result, request)
			if result.Code != tc.status || !strings.HasPrefix(result.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("status %d: %s", result.Code, result.Body.String())
			}
		})
	}
}

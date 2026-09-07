package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func mountedRequest(t *testing.T, app *App, method, path string, body []byte, accept string, signed bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://relay.test/r/main"+path, bytes.NewReader(body))
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if signed {
		signRequest(t, req, string(body))
	}
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)
	return res
}

func TestMountedRouteNIP11AndBridgeAcceptance(t *testing.T) {
	app, _ := testTenant(t)
	info := mountedRequest(t, app, http.MethodGet, "/", nil, "application/nostr+json", false)
	if info.Code != http.StatusOK || info.Header().Get("Content-Type") != "application/nostr+json" {
		t.Fatalf("NIP-11 status/content-type=%d/%q", info.Code, info.Header().Get("Content-Type"))
	}
	var document map[string]any
	if err := json.Unmarshal(info.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if _, ok := document["fuel"]; ok {
		t.Fatal("NIP-11 exposes removed fuel capability")
	}

	query := []byte(`[{"kinds":[1]}]`)
	unauth := mountedRequest(t, app, http.MethodPost, "/query", query, "", false)
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned bridge query=%d", unauth.Code)
	}
	auth := mountedRequest(t, app, http.MethodPost, "/query", query, "", true)
	if auth.Code != http.StatusOK {
		t.Fatalf("signed bridge query=%d body=%s", auth.Code, auth.Body.String())
	}
	bad := mountedRequest(t, app, http.MethodPost, "/query", []byte(`[{`), "", true)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("malformed bridge query=%d", bad.Code)
	}
}

func TestMountedRouteNIP86CORSAndPublicPages(t *testing.T) {
	app, _ := testTenant(t)
	body := []byte(`{"method":"listviews","params":[]}`)
	unauthReq := httptest.NewRequest(http.MethodPost, "http://relay.test/r/main/manage", bytes.NewReader(body))
	unauthReq.Header.Set("Content-Type", "application/nostr+json+rpc")
	unauthRes := httptest.NewRecorder()
	app.ServeHTTP(unauthRes, unauthReq)
	unauth := unauthRes
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned NIP-86=%d", unauth.Code)
	}
	authReq := httptest.NewRequest(http.MethodPost, "http://relay.test/r/main/manage", bytes.NewReader(body))
	authReq.Header.Set("Content-Type", "application/nostr+json+rpc")
	signRequest(t, authReq, string(body))
	authRes := httptest.NewRecorder()
	app.ServeHTTP(authRes, authReq)
	auth := authRes
	if auth.Code != http.StatusOK {
		t.Fatalf("signed NIP-86=%d body=%s", auth.Code, auth.Body.String())
	}
	for _, path := range []string{"/", "/people", "/card.json", "/connect.json", "/terms", "/invite/missing"} {
		res := mountedRequest(t, app, http.MethodGet, path, nil, "", path == "/people" || path == "/connect.json")
		if res.Code != http.StatusOK && !(path == "/invite/missing" && res.Code == http.StatusNotFound) {
			t.Fatalf("public route %s=%d body=%s", path, res.Code, res.Body.String())
		}
	}
	preflight := mountedRequest(t, app, http.MethodOptions, "/query", nil, "", false)
	if preflight.Code != http.StatusNoContent || preflight.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("CORS preflight=%d origin=%q", preflight.Code, preflight.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestMountedRouteNIP05WebAddressAndDataAuth(t *testing.T) {
	app, tenant := testTenant(t)
	p := tenant.Policy()
	p.Features.Names = true
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	unknown := mountedRequest(t, app, http.MethodGet, "/.well-known/nostr.json?name=missing", nil, "", false)
	if unknown.Code != http.StatusOK {
		t.Fatalf("unknown NIP-05=%d", unknown.Code)
	}
	var emptyNIP05 struct {
		Names map[string]string `json:"names"`
	}
	if err := json.Unmarshal(unknown.Body.Bytes(), &emptyNIP05); err != nil || len(emptyNIP05.Names) != 0 {
		t.Fatalf("unknown NIP-05 document=%s", unknown.Body.String())
	}
	web := mountedRequest(t, app, http.MethodGet, "/.well-known/nostr.json?path=/npub1missing", nil, "", false)
	if web.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned NIP-AD=%d", web.Code)
	}
	importRes := mountedRequest(t, app, http.MethodPut, "/import", []byte("{}\n"), "", false)
	if importRes.Code != http.StatusForbidden {
		t.Fatalf("unsigned import=%d", importRes.Code)
	}
	backup := mountedRequest(t, app, http.MethodGet, "/backups/missing.json", nil, "", false)
	if backup.Code != http.StatusUnauthorized && backup.Code != http.StatusForbidden && backup.Code != http.StatusNotFound {
		t.Fatalf("backup visibility=%d", backup.Code)
	}
	if tenant.Policy().Features.Files {
		// The dedicated NIP-96 test covers upload/delete; this assertion keeps
		// the mounted route's disabled-file behavior explicit for this matrix.
		p := tenant.Policy()
		p.Features.Files = false
		if err := tenant.applyPolicy(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		blob := mountedRequest(t, app, http.MethodGet, "/nip96/missing", nil, "", false)
		if blob.Code != http.StatusNotFound {
			t.Fatalf("disabled blob route=%d", blob.Code)
		}
	}
}

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These are the exact argument shapes emitted by the guided Sync forms. The
// test is deliberately service-level so it catches UI/backend drift without
// depending on template markup.
func TestGuidedSyncArgumentContracts(t *testing.T) {
	ctx := context.Background()
	_, tenant := testTenant(t)
	filter := json.RawMessage(`{"kinds":[1],"limit":10}`)
	if _, err := tenant.replication.ExecuteRaw(ctx, "pullfrom", []json.RawMessage{json.RawMessage(`"https://relay.example"`), filter}); err != nil {
		t.Fatalf("pullfrom guided args: %v", err)
	}
	if _, err := tenant.replication.ExecuteRaw(ctx, "backfill", []json.RawMessage{json.RawMessage(`{"relays":["https://relay.example"]}`)}); err != nil {
		t.Fatalf("backfill guided args: %v", err)
	}
	if _, err := tenant.community.Execute(ctx, tenant.Policy().Owner, "createinvite", []json.RawMessage{json.RawMessage(`259200`), json.RawMessage(`1`), json.RawMessage(`note`)}); err != nil {
		t.Fatalf("createinvite guided args: %v", err)
	}
	if _, err := tenant.community.Execute(ctx, tenant.Policy().Owner, "setmember", []json.RawMessage{json.RawMessage(`"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`), json.RawMessage(`{"name":"reader","note":"hello"}`)}); err != nil {
		t.Fatalf("setmember guided args: %v", err)
	}
}

func TestSignedManagementJourneys(t *testing.T) {
	ctx := context.Background()
	app, tenant := testTenant(t)
	owner := tenant.Policy().Owner
	call := func(method string, params ...any) map[string]any {
		t.Helper()
		rawParams := make([]json.RawMessage, len(params))
		for i, value := range params {
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			rawParams[i] = raw
		}
		body, err := json.Marshal(map[string]any{"method": method, "params": rawParams})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "http://relay.test/", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/nostr+json+rpc")
		signRequest(t, req, string(body))
		res := httptest.NewRecorder()
		tenant.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", method, res.Code, res.Body.String())
		}
		var response map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	invite := call("createinvite", 3600, 1, "guided")
	if invite["result"] == nil {
		t.Fatal("createinvite returned no result")
	}
	call("listinvites")
	member := strings.Repeat("2", 64)
	call("setmember", member, map[string]any{"name": "reader", "note": "guided"})
	call("banpubkey", member, "test")
	call("allowpubkey", member, "restored")
	call("blockip", "192.0.2.1", "test")
	call("unblockip", "192.0.2.1")
	call("setpolicy", map[string]any{"description": "guided"})
	call("changerelayname", "guided relay")
	call("changerelaydescription", "guided description")
	call("publishview", "articles")
	call("exportconfig")
	call("forkrelay", map[string]any{"name": "guided-fork", "owner": owner, "people": true})
	call("removemember", member)

	// Deletion has its own tenant so it cannot invalidate the remaining journey.
	meta, err := app.Create(ctx, CreateOptions{Name: "guided-delete", Owner: owner, Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	deleteTenant, err := app.tenant(ctx, meta, "http://relay.test/r/guided-delete")
	if err != nil {
		t.Fatal(err)
	}
	deleteBody, _ := json.Marshal(map[string]any{"method": "deleterelay", "params": []json.RawMessage{json.RawMessage(`"guided-delete"`)}})
	deleteReq := httptest.NewRequest(http.MethodPost, "http://relay.test/", bytes.NewReader(deleteBody))
	deleteReq.Header.Set("Content-Type", "application/nostr+json+rpc")
	signRequest(t, deleteReq, string(deleteBody))
	deleteRes := httptest.NewRecorder()
	deleteTenant.ServeHTTP(deleteRes, deleteReq)
	if deleteRes.Code != http.StatusOK {
		t.Fatalf("deleterelay status=%d body=%s", deleteRes.Code, deleteRes.Body.String())
	}
}

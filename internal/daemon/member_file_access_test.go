package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMemberFileAccessAcrossHTTPAndBrowseSurfaces(t *testing.T) {
	h := newRoomHarness(t)
	ctx := context.Background()
	const body = "members-only file bytes"
	request := httptest.NewRequest(http.MethodPut, "http://relay.test/upload?access=members&filename=private.txt", strings.NewReader(body))
	request.Header.Set("Content-Type", "text/plain")
	signRequestWithSecret(t, request, body, h.secrets["alice"])
	uploaded := httptest.NewRecorder()
	h.tenant.ServeHTTP(uploaded, request)
	if uploaded.Code != http.StatusOK {
		t.Fatalf("member upload = %d: %s", uploaded.Code, uploaded.Body.String())
	}
	var descriptor struct {
		Hash string `json:"sha256"`
	}
	if err := json.Unmarshal(uploaded.Body.Bytes(), &descriptor); err != nil || len(descriptor.Hash) != 64 {
		t.Fatalf("upload descriptor = %s: %v", uploaded.Body.String(), err)
	}

	if rows, err := h.browse("bob", "browsefiles", map[string]any{"view": "library", "limit": 50}); err != nil {
		t.Fatalf("member library: %v", err)
	} else if !libraryContainsHash(rows, descriptor.Hash, "private.txt") {
		t.Fatalf("member library omitted private file: %v", rows)
	}
	if rows, err := h.browse("stranger", "browsefiles", map[string]any{"view": "library", "limit": 50}); err != nil {
		t.Fatalf("outsider library: %v", err)
	} else if libraryContainsHash(rows, descriptor.Hash, "private.txt") {
		t.Fatalf("outsider library exposed private file: %v", rows)
	}

	for _, route := range []string{"/" + descriptor.Hash, "/nip96/" + descriptor.Hash, "/files/raw?hash=" + descriptor.Hash, "/media/" + descriptor.Hash} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			for _, actor := range []string{"owner", "bob"} {
				t.Run(method+" "+actor+" "+route, func(t *testing.T) {
					response := memberFileRequest(t, h, method, route, actor)
					if response.Code != http.StatusOK {
						t.Fatalf("%s response = %d: %s", actor, response.Code, response.Body.String())
					}
					if method == http.MethodGet && response.Body.String() != body {
						t.Fatalf("%s body = %q", actor, response.Body.String())
					}
				})
			}
			t.Run(method+" guest "+route, func(t *testing.T) {
				response := memberFileRequest(t, h, method, route, "")
				if response.Code != http.StatusUnauthorized && response.Code != http.StatusForbidden {
					t.Fatalf("guest response = %d: %s", response.Code, response.Body.String())
				}
			})
			t.Run(method+" outsider "+route, func(t *testing.T) {
				response := memberFileRequest(t, h, method, route, "stranger")
				if response.Code != http.StatusForbidden {
					t.Fatalf("outsider response = %d: %s", response.Code, response.Body.String())
				}
			})
		}
	}

	for _, actor := range []string{"bob", "stranger"} {
		request := httptest.NewRequest(http.MethodGet, "http://relay.test/file?hash="+descriptor.Hash, nil)
		if actor != "" {
			signRequestWithSecret(t, request, "", h.secrets[actor])
		}
		response := httptest.NewRecorder()
		h.tenant.ServeHTTP(response, request)
		page := response.Body.String()
		if actor == "bob" {
			if response.Code != http.StatusOK || !strings.Contains(page, "private.txt") {
				t.Fatalf("member file page = %d: %s", response.Code, page)
			}
		} else if strings.Contains(page, "private.txt") || strings.Contains(page, body) {
			t.Fatalf("outsider file page leaked private data: %d: %s", response.Code, page)
		}
	}

	result, err := h.tenant.Execute(ctx, h.keys["bob"], "browsefile", []json.RawMessage{rawJSON(map[string]any{"hash": descriptor.Hash})})
	if err != nil || !strings.Contains(string(rawJSON(result)), "private.txt") {
		t.Fatalf("member browsefile = %v, %v", result, err)
	}
	if _, err := h.tenant.Execute(ctx, h.keys["stranger"], "browsefile", []json.RawMessage{rawJSON(map[string]any{"hash": descriptor.Hash})}); err == nil {
		t.Fatal("outsider browsefile exposed private file")
	}

	if _, err := h.tenant.Execute(ctx, h.keys["owner"], "removemember", []json.RawMessage{rawJSON(h.keys["bob"])}); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	if response := memberFileRequest(t, h, http.MethodGet, "/files/raw?hash="+descriptor.Hash+"&check=revoked", "bob"); response.Code != http.StatusForbidden {
		t.Fatalf("revoked member response = %d: %s", response.Code, response.Body.String())
	}
	if _, err := h.tenant.Execute(ctx, h.keys["owner"], "setmember", []json.RawMessage{rawJSON(h.keys["bob"]), rawJSON(map[string]any{"role": "member"})}); err != nil {
		t.Fatalf("restore member: %v", err)
	}
	if _, err := h.tenant.Execute(ctx, h.keys["owner"], "banpubkey", []json.RawMessage{rawJSON(h.keys["bob"]), rawJSON("test")}); err != nil {
		t.Fatalf("ban member: %v", err)
	}
	if response := memberFileRequest(t, h, http.MethodGet, "/files/raw?hash="+descriptor.Hash+"&check=banned", "bob"); response.Code != http.StatusForbidden {
		t.Fatalf("banned member response = %d: %s", response.Code, response.Body.String())
	}
}

func memberFileRequest(t *testing.T, h *roomHarness, method, route, actor string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "http://relay.test"+route, nil)
	if actor != "" {
		signRequestWithSecret(t, request, "", h.secrets[actor])
	}
	response := httptest.NewRecorder()
	h.tenant.ServeHTTP(response, request)
	return response
}

func libraryContainsHash(rows map[string]any, hash, name string) bool {
	items, ok := rows["items"].([]any)
	if !ok {
		return false
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if ok && item["sha256"] == hash && item["name"] == name {
			return true
		}
	}
	return false
}

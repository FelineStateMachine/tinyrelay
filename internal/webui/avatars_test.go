package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/seedmark"
)

func TestAvatarEndpointRendersWithoutProfileAccess(t *testing.T) {
	backend := &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}
	backend.policy.Features.Grasp08 = true
	app, err := New(backend, Options{})
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("b", 64)
	path := avatarURL(key)
	response := httptest.NewRecorder()
	app.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != http.StatusOK || response.Body.String() != seedmark.Avatar(key) {
		t.Fatalf("avatar: status=%d body=%s", response.Code, response.Body.String())
	}
	if backend.call != "" || response.Header().Get("Content-Type") != "image/svg+xml; charset=utf-8" || !strings.Contains(response.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("avatar accessed relay data or lacks image caching: call=%q headers=%v", backend.call, response.Header())
	}
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		request := httptest.NewRequest(method, path, nil)
		if method == http.MethodGet {
			request.Header.Set("If-None-Match", response.Header().Get("ETag"))
		}
		next := httptest.NewRecorder()
		app.ServeHTTP(next, request)
		if next.Body.Len() != 0 || (method == http.MethodHead && next.Code != http.StatusOK) || (method == http.MethodGet && next.Code != http.StatusNotModified) {
			t.Fatalf("%s cache response: status=%d body=%s", method, next.Code, next.Body.String())
		}
	}
}

func TestAvatarEndpointRejectsOtherVersionsSeedsAndWrites(t *testing.T) {
	app, err := New(&fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/avatars/v2/" + strings.Repeat("a", 64) + ".svg", http.StatusNotFound},
		{http.MethodGet, "/avatars/v1/short.svg", http.StatusNotFound},
		{http.MethodGet, "/avatars/v1/" + strings.Repeat("g", 64) + ".svg", http.StatusNotFound},
		{http.MethodGet, "/avatars/v1/" + strings.Repeat("a", 64) + ".png", http.StatusNotFound},
		{http.MethodPost, avatarURL(strings.Repeat("a", 64)), http.StatusMethodNotAllowed},
	} {
		response := httptest.NewRecorder()
		app.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
		if response.Code != tc.status {
			t.Errorf("%s %s: status=%d, want %d", tc.method, tc.path, response.Code, tc.status)
		}
	}
}

func TestAvatarSeedsUseFullKeysAndRoomIdentity(t *testing.T) {
	key := strings.Repeat("ab", 32)
	if avatarURL(strings.ToUpper(key)) != avatarURL(key) || avatarURL(key) == avatarURL(key[:62]+"cd") {
		t.Fatal("profile seed must use the full normalized key")
	}
	if avatarURL("invalid") != "/avatars/v1/"+strings.Repeat("0", 64)+".svg" {
		t.Fatal("invalid keys should use the anonymous fallback")
	}
	room := roomAvatarURL("https://relay.test/r/team", "general")
	if room != roomAvatarURL("https://relay.test/r/team/", "general") || room == roomAvatarURL("https://relay.test/r/other", "general") || room == roomAvatarURL("https://relay.test/r/team", "random") {
		t.Fatal("room avatars must follow stable relay and room identity")
	}
}

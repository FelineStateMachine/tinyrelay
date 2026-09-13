package tinygit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/tinygit"
)

func signedAnnouncement(t *testing.T, secret string, tags ...[]string) tinygit.Event {
	t.Helper()
	e := tinygit.Event{Kind: 30617, CreatedAt: 100, Tags: append([][]string{{"d", "demo"}}, tags...)}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	return e
}

func postEvent(t *testing.T, s http.Handler, e tinygit.Event) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader(body)))
	return w
}

func TestStandaloneServerPersistsSignedAnnouncement(t *testing.T) {
	e := signedAnnouncement(t, strings.Repeat("0", 63)+"1")
	cfg := tinygit.ServerConfig{DataDir: t.TempDir(), Owner: e.PubKey}
	s, err := tinygit.OpenServer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	w := postEvent(t, s, e)
	if w.Code != http.StatusOK {
		t.Fatalf("publish = %d %s", w.Code, w.Body.String())
	}
	// Retrying a signed event is safe even if the caller lost the first response.
	if w := postEvent(t, s, e); w.Code != http.StatusOK {
		t.Fatalf("retry = %d %s", w.Code, w.Body.String())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = tinygit.OpenServer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Bech32 public key for the test's secret scalar 1.
	npub := "npub10xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqpkge6d"
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+npub+"/demo.git/info/refs?service=git-upload-pack", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "git-upload-pack-advertisement") {
		t.Fatalf("reopened Git discovery = %d %s", w.Code, w.Body.String())
	}
}

func TestStandaloneServerRestrictsRepositoryAdmission(t *testing.T) {
	ownerEvent := signedAnnouncement(t, strings.Repeat("0", 63)+"1")
	s, err := tinygit.OpenServer(context.Background(), tinygit.ServerConfig{DataDir: t.TempDir(), Owner: ownerEvent.PubKey})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tc := range []struct {
		name string
		e    tinygit.Event
	}{
		{"foreign owner", signedAnnouncement(t, strings.Repeat("0", 63)+"2")},
		{"private repository", signedAnnouncement(t, strings.Repeat("0", 63)+"1", []string{"private", "true"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := postEvent(t, s, tc.e); w.Code != http.StatusForbidden {
				t.Fatalf("admission = %d %s", w.Code, w.Body.String())
			}
		})
	}
	if w := postEvent(t, s, ownerEvent); w.Code != http.StatusOK {
		t.Fatalf("owner's public announcement = %d %s", w.Code, w.Body.String())
	}
}

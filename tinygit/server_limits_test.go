package tinygit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/tinygit"
)

func TestStandaloneServerRequiresExplicitValidConfiguration(t *testing.T) {
	t.Chdir(t.TempDir())
	owner := signedAnnouncement(t, strings.Repeat("0", 63)+"1").PubKey
	for _, tc := range []struct {
		name string
		cfg  tinygit.ServerConfig
	}{
		{"missing owner", tinygit.ServerConfig{DataDir: t.TempDir()}},
		{"malformed owner", tinygit.ServerConfig{DataDir: t.TempDir(), Owner: "not-a-key"}},
		{"uppercase owner", tinygit.ServerConfig{DataDir: t.TempDir(), Owner: strings.ToUpper(owner)}},
		{"missing data", tinygit.ServerConfig{Owner: owner}},
		{"invalid URL", tinygit.ServerConfig{DataDir: t.TempDir(), Owner: owner, PublicURL: "file:///tmp/git"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := tinygit.OpenServer(context.Background(), tc.cfg)
			if err == nil {
				s.Close()
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestStandaloneServerBoundsEventHTTP(t *testing.T) {
	owner := signedAnnouncement(t, strings.Repeat("0", 63)+"1").PubKey
	s, err := tinygit.OpenServer(context.Background(), tinygit.ServerConfig{DataDir: filepath.Join(t.TempDir(), "data"), Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tc := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"health", http.MethodGet, "/healthz", "", http.StatusOK},
		{"method", http.MethodGet, "/events", "", http.StatusMethodNotAllowed},
		{"body limit", http.MethodPost, "/events", strings.Repeat(" ", (1<<20)+1), http.StatusRequestEntityTooLarge},
		{"unsigned", http.MethodPost, "/events", `{}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			if w.Code != tc.status {
				t.Fatalf("HTTP = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
}

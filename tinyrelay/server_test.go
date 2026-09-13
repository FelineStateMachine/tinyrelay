package tinyrelay_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/tinyrelay"
)

func TestStandaloneCloseDrainsWebSockets(t *testing.T) {
	s, _, client := openRelay(t, false)
	client.send("REQ", "open", map[string]any{"kinds": []int{1}})
	client.expect("EOSE", 2)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := client.conn.Read(ctx); err == nil {
		t.Fatal("connection remained open after server close")
	} else if ctx.Err() != nil {
		t.Fatal("close did not disconnect the client")
	}
}

func TestStandaloneServerRoutesAndClose(t *testing.T) {
	s, err := tinyrelay.OpenServer(context.Background(), tinyrelay.ServerConfig{DataDir: t.TempDir(), PublicURL: "https://relay.example/nostr"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, path := range []string{"/", "/nostr", "/healthz", "/readyz"} {
		response := httptest.NewRecorder()
		s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Errorf("%s: %d", path, response.Code)
		}
	}
	for _, path := range []string{"/git", "/mcp", "/rooms"} {
		response := httptest.NewRecorder()
		s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("%s: %d", path, response.Code)
		}
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	response := httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed server: %d", response.Code)
	}
}

func TestStandaloneServerRejectsInvalidConfig(t *testing.T) {
	for _, publicURL := range []string{"", "file:///tmp/relay", "wss://user:pass@relay.example", "wss://relay.example?token=x", "wss://relay.example#fragment"} {
		t.Run(publicURL, func(t *testing.T) {
			s, err := tinyrelay.OpenServer(context.Background(), tinyrelay.ServerConfig{DataDir: t.TempDir(), PublicURL: publicURL})
			if err == nil {
				s.Close()
				t.Fatal("accepted invalid public URL")
			}
		})
	}
}

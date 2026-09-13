package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/tinyclient"
)

func TestCommandRequiresBackendAndPublicOrigin(t *testing.T) {
	for _, args := range [][]string{nil, {"-backend", "http://relay.example"}, {"-backend", "http://relay.example", "-public-url", "not-a-url"}, {"-backend", "http://relay.example/r/one", "-public-url", "http://client.example/r/two"}} {
		if _, err := configure(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted incomplete or invalid configuration: %v", args)
		}
	}
}

func TestCommandServesEmbeddedClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tinyclient.BackendPath {
			t.Errorf("unexpected upstream page %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("method") != "" {
			_, _ = w.Write([]byte("null"))
			return
		}
		_, _ = w.Write([]byte(`{"policy":{"name":"Standalone frontend","features":{"pages":true}},"slug":"test","url":"http://client.example"}`))
	}))
	defer upstream.Close()
	server, err := configure([]string{"-listen", "127.0.0.1:0", "-backend", upstream.URL, "-public-url", "http://client.example"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, httptest.NewRequest("GET", "http://client.example/", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), "Standalone frontend") {
		t.Fatalf("standalone command: %d %s", response.Code, response.Body.String())
	}
	if server.Addr != "127.0.0.1:0" {
		t.Fatalf("listen=%s", server.Addr)
	}
}

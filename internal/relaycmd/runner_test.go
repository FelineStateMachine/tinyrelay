package relaycmd

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

func TestParseDefaultsAndEnvironment(t *testing.T) {
	t.Setenv("TINY_RELAY_DATA_DIR", "/tmp/relay-data")
	opts, err := parse([]string{"--name", "Example"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.dataDir != "/tmp/relay-data" || opts.listen != "127.0.0.1:7447" {
		t.Fatalf("defaults = %#v", opts)
	}
	if opts.maxMessageBytes != 1<<20 || opts.maxPendingBytes != 4<<20 {
		t.Fatalf("limits = %#v", opts)
	}
	if opts.name != "Example" {
		t.Fatalf("name = %q", opts.name)
	}
}

func TestParseNormalizesRelayURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"http://relay.example/", "ws://relay.example"},
		{"https://relay.example/nostr/", "wss://relay.example/nostr"},
		{"ws://relay.example", "ws://relay.example"},
		{"wss://relay.example/", "wss://relay.example"},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			opts, err := parse([]string{"--public-url", test.input})
			if err != nil {
				t.Fatal(err)
			}
			if opts.publicURL != test.want {
				t.Fatalf("public URL = %q, want %q", opts.publicURL, test.want)
			}
		})
	}
}

func TestParseRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--public-url", "ftp://relay.example"},
		{"--owner", "not-a-public-key"},
		{"--max-message-bytes", "0"},
		{"--max-pending-bytes", "-1"},
		{"extra"},
	} {
		if _, err := parse(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestRunStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &signalWriter{ready: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []string{"--data-dir", t.TempDir(), "--listen", "127.0.0.1:0"}, output)
	}()

	select {
	case <-output.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not stop")
	}
}

type signalWriter struct {
	ready chan struct{}
	once  sync.Once
}

var _ io.Writer = (*signalWriter)(nil)

func (w *signalWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.ready) })
	return len(p), nil
}

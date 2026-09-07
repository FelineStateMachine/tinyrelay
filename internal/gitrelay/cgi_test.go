package gitrelay

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCGIFailureAfterHeadersPreservesPartialPack(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'Content-Type: application/octet-stream\\r\\n\\r\\npartial-pack'\nprintf 'backend failed' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	g := &GitRelay{root: dir}
	w := httptest.NewRecorder()
	err := g.cgi(httptest.NewRequest(http.MethodGet, "/", nil), w, Repository{Owner: "owner", Identifier: "test"}, "/git-upload-pack")
	var streamed *cgiStreamError
	if !errors.As(err, &streamed) || !strings.Contains(err.Error(), "backend failed") {
		t.Fatalf("backend failure did not retain committed-response state: %v", err)
	}
	if got := w.Body.String(); got != "partial-pack" {
		t.Fatalf("error corrupted pack response: %q", got)
	}
}

func TestCGIGzipInputDecodesAndRemovesSpool(t *testing.T) {
	want := strings.Repeat("object negotiation\n", 10000)
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := io.WriteString(zw, want); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", &compressed)
	req.Header.Set("Content-Encoding", "gzip")
	body, length, cleanup, err := cgiInput(req)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	got, err := io.ReadAll(body)
	if err != nil || string(got) != want || length != strconv.Itoa(len(want)) {
		t.Fatalf("decoded request: length=%s bytes=%d err=%v", length, len(got), err)
	}
	file, ok := body.(*os.File)
	if !ok {
		t.Fatal("compressed request did not use a disk spool")
	}
	cleanup()
	if _, err := os.Stat(file.Name()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary request remains after cleanup: %v", err)
	}
}

// A backend that waits for more input after its first output must not require
// the whole pack to fit in memory before the client can receive any of it.
func TestCGIStreamsBeforeBackendExits(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'Content-Type: application/octet-stream\\r\\n\\r\\nfirst'\nread release\nprintf 'last'\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	input, release := io.Pipe()
	defer input.Close()
	defer release.Close()
	g := &GitRelay{root: dir}
	finished := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = input
		finished <- g.cgi(r, w, Repository{Owner: "owner", Identifier: "test"}, "/git-upload-pack")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(req)
	if err != nil {
		_ = release.Close()
		t.Fatalf("backend output was not streamed: %v", err)
	}
	defer response.Body.Close()
	first := make([]byte, 5)
	if _, err := io.ReadFull(response.Body, first); err != nil || string(first) != "first" {
		_ = release.Close()
		t.Fatalf("first backend chunk = %q, %v", first, err)
	}
	if _, err := io.WriteString(release, "continue\n"); err != nil {
		t.Fatal(err)
	}
	_ = release.Close()
	rest, err := io.ReadAll(response.Body)
	if err != nil || string(rest) != "last" {
		t.Fatalf("remaining backend output = %q, %v", rest, err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

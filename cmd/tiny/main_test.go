package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestCommandNameAndPermanentTenant(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"help"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "tiny serve") || strings.Contains(out.String(), "tinyrelay serve") {
		t.Fatal(out.String())
	}
	args := []string{"tenant", "create", "--data-dir", t.TempDir(), "--name", "main", "--owner", strings.Repeat("a", 64)}
	if err := run(context.Background(), args, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ready") {
		t.Fatal(out.String())
	}
}

func TestServerShutdownDoesNotHideAnotherFailure(t *testing.T) {
	results := make(chan error, 1)
	results <- http.ErrServerClosed
	want := errors.New("diagnostics listener failed")
	go func() { results <- want }()
	err := awaitServers(context.Background(), []*http.Server{{}, {}}, results)
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

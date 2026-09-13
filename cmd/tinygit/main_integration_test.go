package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// This is deliberately a subprocess acceptance test: it proves cmd/tinygit is
// a real standalone server, not only an embeddable HTTP handler.
func TestStandaloneBinarySignedPushCloneAndRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the standalone binary and native Git")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "tinygit")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build standalone binary: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	secret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "data")
	start := func() (string, func()) {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, "-listen", "127.0.0.1:0", "-data", data, "-owner", owner)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		stopped := false
		stop := func() {
			t.Helper()
			if stopped {
				return
			}
			stopped = true
			_ = cmd.Process.Signal(os.Interrupt)
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("server exit: %v", err)
				}
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				<-done
				t.Error("server did not shut down")
			}
		}
		t.Cleanup(stop)
		line := make(chan string, 1)
		go func() {
			scanner := bufio.NewScanner(stdout)
			if scanner.Scan() {
				line <- scanner.Text()
			} else {
				line <- ""
			}
		}()
		var address string
		select {
		case ready := <-line:
			const prefix = "tinygit listening on "
			if !strings.HasPrefix(ready, prefix) {
				t.Fatalf("startup: %q", ready)
			}
			address = strings.TrimPrefix(ready, prefix)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		res, err := http.Get(address + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("health = %d", res.StatusCode)
		}
		return address, stop
	}
	address, stop := start()
	post := func(e event.Event) bool {
		t.Helper()
		if err := event.Sign(&e, secret); err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.Post(address+"/events", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var result struct {
			ID      string `json:"id"`
			Pending bool   `json:"pending"`
		}
		if res.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(res.Body)
			t.Fatalf("publish = %d: %s", res.StatusCode, body)
		}
		if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if result.ID != e.ID {
			t.Fatalf("event ID = %q, want %q", result.ID, e.ID)
		}
		return result.Pending
	}
	git := func(dir string, wantSuccess bool, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		out, err := cmd.CombinedOutput()
		if (err == nil) != wantSuccess {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	git(work, true, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "hello.txt"), []byte("standalone Git\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(work, true, "add", "hello.txt")
	git(work, true, "commit", "-m", "first")
	oid := git(work, true, "rev-parse", "HEAD")
	post(event.Event{Kind: 30617, CreatedAt: 100, Tags: [][]string{{"d", "demo"}}})
	cloneURL := func() string {
		return address + "/npub10xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqpkge6d/demo.git"
	}
	git(work, false, "push", cloneURL(), "main")
	state := event.Event{Kind: 30618, CreatedAt: 101, Tags: [][]string{{"d", "demo"}, {"HEAD", "ref: refs/heads/main"}, {"refs/heads/main", oid}}}
	if !post(state) {
		t.Fatal("missing objects were not staged as pending")
	}
	t.Log(git(work, true, "push", cloneURL(), "main"))
	if post(state) {
		t.Fatal("receive-pack did not promote pending state")
	}
	git(work, true, "commit", "--allow-empty", "-m", "unsigned change")
	git(work, false, "push", cloneURL(), "main")
	git(work, false, "push", cloneURL(), "HEAD:refs/heads/unauthorized")
	verifyClone := func(name string) {
		t.Helper()
		dest := filepath.Join(root, name)
		t.Log(git(root, true, "clone", cloneURL(), dest))
		if got := git(dest, true, "rev-parse", "HEAD"); got != oid {
			t.Fatalf("cloned HEAD = %s, want %s", got, oid)
		}
		body, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
		if err != nil || string(body) != "standalone Git\n" {
			t.Fatalf("clone content = %q (%v)", body, err)
		}
	}
	verifyClone("clone")
	stop()
	address, stop = start()
	defer stop()
	verifyClone("restarted-clone")
	t.Log(fmt.Sprintf("verified signed push, rejected unsigned pushes, clone and durable restart at %s", oid))
}

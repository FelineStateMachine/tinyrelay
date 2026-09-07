package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/replication"
)

func TestMountedHTTPBackupRestoreIncludesGitInventory(t *testing.T) {
	ctx := context.Background()
	_, source := testTenant(t)
	app, target := testTenant(t)
	repoPath := filepath.Join(source.meta.Paths.Git, source.Policy().Owner, "portable.git")
	if err := os.MkdirAll(filepath.Dir(repoPath), 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "init", "--bare", "--initial-branch=main", repoPath).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "init")
	gitRun(t, work, "config", "user.email", "acceptance@example.com")
	gitRun(t, work, "config", "user.name", "Acceptance")
	if err := os.WriteFile(filepath.Join(work, "README.txt"), []byte("portable git text\n"), 0600); err != nil {
		t.Fatal(err)
	}
	binary := []byte{0, 1, 2, 127, 128, 255}
	if err := os.WriteFile(filepath.Join(work, "payload.bin"), binary, 0600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "README.txt", "payload.bin")
	gitRun(t, work, "commit", "-m", "portable inventory")
	gitRun(t, work, "branch", "-M", "main")
	gitRun(t, work, "tag", "v1")
	gitRun(t, work, "remote", "add", "origin", repoPath)
	gitRun(t, work, "push", "--all", "origin")
	gitRun(t, work, "push", "--tags", "origin")
	commit := strings.TrimSpace(string(backupGitOutput(t, work, "rev-parse", "HEAD")))
	binaryOID := strings.TrimSpace(string(backupGitOutput(t, work, "rev-parse", "HEAD:payload.bin")))
	archive, err := replication.CreateBackupWithProvider(ctx, source.store, source.ReplicationBackupProvider(), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(archive)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://relay.test/r/main/backups/restore", bytes.NewReader(body))
	signRequest(t, req, string(body))
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("mounted Git restore %d: %s", res.Code, res.Body.String())
	}
	restoredRepo := filepath.Join(target.meta.Paths.Git, source.Policy().Owner, "portable.git")
	if got := strings.TrimSpace(string(backupGitOutput(t, restoredRepo, "rev-parse", "refs/heads/main"))); got != commit {
		t.Fatalf("restored main ref = %s, want %s", got, commit)
	}
	if got := strings.TrimSpace(string(backupGitOutput(t, restoredRepo, "rev-parse", "refs/tags/v1"))); got != commit {
		t.Fatalf("restored tag ref = %s, want %s", got, commit)
	}
	if got := string(backupGitOutput(t, restoredRepo, "show", "HEAD:README.txt")); got != "portable git text\n" {
		t.Fatalf("restored text = %q", got)
	}
	if got := strings.TrimSpace(string(backupGitOutput(t, restoredRepo, "rev-parse", "HEAD:payload.bin"))); got != binaryOID {
		t.Fatalf("restored binary oid = %s, want %s", got, binaryOID)
	}
	if output, err := exec.Command("git", "--git-dir", restoredRepo, "cat-file", "blob", "HEAD:payload.bin").Output(); err != nil || string(output) != string(binary) {
		t.Fatalf("restored binary content mismatch: %v", err)
	}
	if output, err := exec.Command("git", "--git-dir", restoredRepo, "fsck", "--full", "--no-progress").CombinedOutput(); err != nil {
		t.Fatalf("restored Git fsck: %v: %s", err, output)
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func backupGitOutput(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	command := exec.Command("git", append([]string{"--git-dir", dir}, args...)...)
	if dir != "" && !strings.HasSuffix(dir, ".git") {
		command = exec.Command("git", append([]string{"-C", dir}, args...)...)
	}
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return output
}

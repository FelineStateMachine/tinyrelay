package gitrelay

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestFetchMissingHTTPSRepairAndNoop(t *testing.T) {
	root := t.TempDir()
	remoteRoot := filepath.Join(root, "remote")
	work := filepath.Join(root, "work")
	gitRepairRun(t, "init", "--bare", filepath.Join(remoteRoot, "repo.git"))
	gitRepairRun(t, "init", work)
	gitRepairRun(t, "-C", work, "config", "user.email", "repair@example.test")
	gitRepairRun(t, "-C", work, "config", "user.name", "Repair")
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("repair\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitRepairRun(t, "-C", work, "add", "README")
	gitRepairRun(t, "-C", work, "commit", "-m", "repair")
	commit := strings.TrimSpace(gitRepairOutput(t, "-C", work, "rev-parse", "HEAD"))
	gitRepairRun(t, "-C", work, "push", filepath.Join(remoteRoot, "repo.git"), "HEAD:refs/heads/main")

	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		serveGitBackend(t, w, r, remoteRoot)
	}))
	t.Cleanup(server.Close)
	caFile := filepath.Join(root, "ca.pem")
	cert, err := x509.ParseCertificate(server.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", caFile)

	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	p := policy.Defaults("")
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git"), Policy: func() policy.Policy { return p }, AllowPrivateRelays: true})
	if err != nil {
		t.Fatal(err)
	}
	repo := Repository{Owner: strings.Repeat("a", 64), Identifier: "repo", Clone: []string{server.URL + "/repo.git"}, Refs: map[string]string{"refs/heads/main": commit}}
	if err := g.FetchMissing(context.Background(), repo, repo.Clone, repo.Refs); err != nil {
		t.Fatal(err)
	}
	if requests.Load() == 0 || !gitRepairObject(t, filepath.Join(root, "git", repo.Owner, "repo.git"), commit) {
		t.Fatalf("repair did not fetch commit: requests=%d", requests.Load())
	}
	firstRequests := requests.Load()
	if err := g.FetchMissing(context.Background(), repo, repo.Clone, repo.Refs); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != firstRequests {
		t.Fatalf("noop repair made network requests: before=%d after=%d", firstRequests, requests.Load())
	}
}

func serveGitBackend(t *testing.T, w http.ResponseWriter, r *http.Request, root string) {
	t.Helper()
	env := append(os.Environ(), "GIT_PROJECT_ROOT="+root, "GIT_HTTP_EXPORT_ALL=1", "PATH_INFO="+r.URL.Path, "REQUEST_METHOD="+r.Method, "QUERY_STRING="+r.URL.RawQuery, "CONTENT_TYPE="+r.Header.Get("Content-Type"), "CONTENT_LENGTH="+strconv.FormatInt(r.ContentLength, 10))
	cmd := exec.Command("git", "http-backend")
	cmd.Env = env
	cmd.Stdin = r.Body
	out, err := cmd.Output()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	parts := strings.SplitN(string(out), "\r\n\r\n", 2)
	if len(parts) != 2 {
		http.Error(w, "invalid git backend response", http.StatusBadGateway)
		return
	}
	for _, line := range strings.Split(parts[0], "\r\n") {
		fields := strings.SplitN(line, ": ", 2)
		if len(fields) == 2 {
			w.Header().Set(fields[0], fields[1])
		}
	}
	_, _ = io.WriteString(w, parts[1])
}

func gitRepairRun(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
func gitRepairOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
func gitRepairObject(t *testing.T, dir, object string) bool {
	t.Helper()
	return exec.Command("git", "--git-dir", dir, "cat-file", "-e", object).Run() == nil
}

package gitrelay

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestPullRequestTargetUsesEventIDForStandardNIP34(t *testing.T) {
	secret := strings.Repeat("0", 63) + "1"
	root := event.Event{Kind: event.KIND_GIT_PR, CreatedAt: 1, Content: "change", Tags: [][]string{{"a", "30617:owner:repo"}, {"c", strings.Repeat("a", 40)}, {"clone", "https://git.example/repo.git"}}}
	if err := event.Sign(&root, secret); err != nil {
		t.Fatal(err)
	}
	r, err := pullRequestTarget(root, event.KIND_GIT_PR)
	if err != nil {
		t.Fatal(err)
	}
	if r.Identifier != root.ID || r.Refs["refs/nostr/"+root.ID] != strings.Repeat("a", 40) {
		t.Fatalf("standard PR target = %#v", r)
	}
}

func TestPullRequestUpdateRequiresRootAuthor(t *testing.T) {
	rootSecret := strings.Repeat("0", 63) + "1"
	otherSecret := strings.Repeat("0", 63) + "2"
	root := event.Event{Kind: event.KIND_GIT_PR, CreatedAt: 1, Tags: [][]string{{"a", "30617:owner:repo"}, {"c", strings.Repeat("a", 40)}, {"clone", "https://git.example/repo.git"}}}
	if err := event.Sign(&root, rootSecret); err != nil {
		t.Fatal(err)
	}
	update := event.Event{Kind: event.KIND_GIT_PR_UPDATE, CreatedAt: 2, Tags: [][]string{{"E", root.ID}, {"P", root.PubKey}, {"c", strings.Repeat("b", 40)}, {"clone", "https://git.example/repo.git"}}}
	if err := event.Sign(&update, otherSecret); err != nil {
		t.Fatal(err)
	}
	if err := validatePullRequestUpdate(root, update); err == nil {
		t.Fatal("accepted update signed by another author")
	}
}

func TestPullRequestRepairRejectsMissingCloneBeforeGit(t *testing.T) {
	secret := strings.Repeat("0", 63) + "1"
	root := event.Event{Kind: event.KIND_GIT_PR, CreatedAt: 1, Tags: [][]string{{"a", "30617:owner:repo"}, {"c", strings.Repeat("a", 40)}}}
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	root.Tags[0][1] = "30617:" + owner + ":repo"
	if err := event.Sign(&root, secret); err != nil {
		t.Fatal(err)
	}
	g := &GitRelay{}
	hosted := Repository{Owner: root.PubKey, Identifier: "repo", Refs: map[string]string{}}
	err = g.RepairPullRequestObjects(context.Background(), hosted, PullRequestRepair{Root: root})
	if err == nil || !strings.Contains(err.Error(), "no clone source") {
		t.Fatalf("missing clone error = %v", err)
	}
}

func TestPullRequestRepairUsesHostedRepositoryRefs(t *testing.T) {
	rootDir := t.TempDir()
	remote := filepath.Join(rootDir, "remote", "repo.git")
	work := filepath.Join(rootDir, "work")
	prGitRun(t, "init", "--bare", remote)
	prGitRun(t, "init", work)
	prGitRun(t, "-C", work, "config", "user.email", "pr@example.test")
	prGitRun(t, "-C", work, "config", "user.name", "PR")
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prGitRun(t, "-C", work, "add", "README")
	prGitRun(t, "-C", work, "commit", "-m", "one")
	first := strings.TrimSpace(prGitOutput(t, "-C", work, "rev-parse", "HEAD"))
	prGitRun(t, "-C", work, "push", remote, "HEAD:refs/heads/main")
	if err := os.WriteFile(filepath.Join(work, "NEXT"), []byte("two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prGitRun(t, "-C", work, "add", "NEXT")
	prGitRun(t, "-C", work, "commit", "-m", "two")
	second := strings.TrimSpace(prGitOutput(t, "-C", work, "rev-parse", "HEAD"))
	prGitRun(t, "-C", work, "push", remote, "HEAD:refs/heads/pr")

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveGitBackend(t, w, r, filepath.Dir(remote)) }))
	t.Cleanup(server.Close)
	cert, err := x509.ParseCertificate(server.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(rootDir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", ca)
	store, err := storage.Open(context.Background(), filepath.Join(rootDir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	p := policy.Defaults("")
	g, err := New(Config{Store: store, Root: filepath.Join(rootDir, "git"), Policy: func() policy.Policy { return p }, AllowPrivateRelays: true})
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	clone := server.URL + "/repo.git"
	root := event.Event{Kind: event.KIND_GIT_PR, CreatedAt: 1, Content: "PR", Tags: [][]string{{"a", "30617:" + owner + ":repo"}, {"c", first}, {"clone", clone}}}
	if err := event.Sign(&root, secret); err != nil {
		t.Fatal(err)
	}
	update := event.Event{Kind: event.KIND_GIT_PR_UPDATE, CreatedAt: 2, Tags: [][]string{{"E", root.ID}, {"P", owner}, {"c", second}, {"clone", clone}}}
	if err := event.Sign(&update, secret); err != nil {
		t.Fatal(err)
	}
	hosted := Repository{Owner: owner, Identifier: "repo", Clone: []string{clone}, Refs: map[string]string{"refs/heads/main": first}}
	if err := g.FetchMissing(context.Background(), hosted, hosted.Clone, hosted.Refs); err != nil {
		t.Fatal(err)
	}
	if g.objectsPresent(context.Background(), hosted, []string{second}) {
		t.Fatal("PR update was already present before repair; test must exercise a real fetch")
	}
	if err := g.RepairPullRequestObjects(context.Background(), hosted, PullRequestRepair{Root: root, Updates: []event.Event{update}}); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(rootDir, "git", owner, "repo.git")
	for ref, want := range map[string]string{"refs/heads/main": first, "refs/nostr/" + root.ID: first, "refs/nostr/" + update.ID: second} {
		got := strings.TrimSpace(prGitOutput(t, "--git-dir", gitDir, "rev-parse", ref))
		if got != want {
			t.Fatalf("%s = %s, want %s", ref, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(gitDir, "tinyrelay.refs")); !os.IsNotExist(err) {
		t.Fatalf("PR repair wrote signed state file: %v", err)
	}
	quarantines, err := filepath.Glob(filepath.Join(rootDir, "git", "prs", owner, "repo.repair-"+root.ID+"-*.git"))
	if err != nil {
		t.Fatal(err)
	}
	if len(quarantines) != 0 {
		t.Fatalf("PR repair quarantine was not removed: %v", quarantines)
	}
	blockedRoot := root
	blockedRoot.CreatedAt = 3
	if err := event.Sign(&blockedRoot, secret); err != nil {
		t.Fatal(err)
	}
	blockedUpdate := event.Event{Kind: event.KIND_GIT_PR_UPDATE, CreatedAt: 4, Tags: [][]string{{"E", blockedRoot.ID}, {"P", owner}, {"c", second}, {"clone", clone}}}
	if err := event.Sign(&blockedUpdate, secret); err != nil {
		t.Fatal(err)
	}
	// A ref-directory collision makes one update fail. The other signed ref
	// must not appear, regardless of map iteration or transaction order.
	blocker := "refs/nostr/" + blockedRoot.ID + "/child"
	prGitRun(t, "--git-dir", gitDir, "update-ref", blocker, first)
	if err := g.RepairPullRequestObjects(context.Background(), hosted, PullRequestRepair{Root: blockedRoot, Updates: []event.Event{blockedUpdate}}); err == nil {
		t.Fatal("repair ignored conflicting ref")
	}
	if err := exec.Command("git", "--git-dir", gitDir, "show-ref", "--verify", "--quiet", "refs/nostr/"+blockedUpdate.ID).Run(); err == nil {
		t.Fatal("failed repair partially promoted its signed refs")
	}
	if got := strings.TrimSpace(prGitOutput(t, "--git-dir", gitDir, "rev-parse", blocker)); got != first {
		t.Fatalf("repair changed blocking ref: %s", got)
	}
	if out := prGitOutput(t, "--git-dir", gitDir, "for-each-ref", "refs/tinyrelay/pr-repair/"); strings.TrimSpace(out) != "" {
		t.Fatalf("failed repair left temporary refs: %s", out)
	}
}

func TestPullRequestRepairFailureLeavesHostedRefsUnchanged(t *testing.T) {
	secret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	rootDir := t.TempDir()
	hosted := Repository{Owner: owner, Identifier: "repo", Refs: map[string]string{
		"refs/heads/main": strings.Repeat("a", 40),
	}}
	g := &GitRelay{root: rootDir, allowPrivate: true, configured: make(map[string]struct{})}
	root := event.Event{Kind: event.KIND_GIT_PR, CreatedAt: 1, PubKey: owner,
		Tags: [][]string{{"a", "30617:" + owner + ":repo"}, {"c", strings.Repeat("b", 40)}, {"clone", "http://127.0.0.1:1/repo.git"}}}
	if err := event.Sign(&root, secret); err != nil {
		t.Fatal(err)
	}
	err = g.RepairPullRequestObjects(context.Background(), hosted, PullRequestRepair{Root: root})
	if err == nil {
		t.Fatal("repair unexpectedly succeeded from unavailable source")
	}
	if _, err := os.Stat(g.repoPath(hosted)); !os.IsNotExist(err) {
		// ensureRepo is allowed to create the host, but no signed ref may be
		// installed after a failed quarantine admission.
		got, _ := exec.Command("git", "--git-dir", g.repoPath(hosted), "show-ref", "refs/nostr/"+root.ID).CombinedOutput()
		if len(got) != 0 {
			t.Fatalf("failed repair installed hosted PR ref: %s", got)
		}
	}
}

func TestPRQuarantineRejectsHistoryBeyondDepth(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	remote := filepath.Join(root, "remote.git")
	prGitRun(t, "init", work)
	prGitRun(t, "-C", work, "config", "user.email", "pr@example.test")
	prGitRun(t, "-C", work, "config", "user.name", "PR")
	for i := 0; i < prRepairDepth+1; i++ {
		if err := os.WriteFile(filepath.Join(work, "history"), []byte(fmt.Sprintf("%d\n", i)), 0600); err != nil {
			t.Fatal(err)
		}
		prGitRun(t, "-C", work, "add", "history")
		prGitRun(t, "-C", work, "commit", "-m", fmt.Sprintf("commit %d", i))
	}
	tip := strings.TrimSpace(prGitOutput(t, "-C", work, "rev-parse", "HEAD"))
	base := strings.TrimSpace(prGitOutput(t, "-C", work, "rev-parse", "HEAD~1"))
	prGitRun(t, "init", "--bare", remote)
	prGitRun(t, "-C", work, "push", remote, "HEAD:refs/heads/main")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveGitBackend(t, w, r, root) }))
	t.Cleanup(server.Close)
	ca := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", ca)
	secret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	hosted := Repository{Owner: owner, Identifier: "repo"}
	g := &GitRelay{root: filepath.Join(root, "hosted"), allowPrivate: true, configured: make(map[string]struct{})}
	pr := event.Event{Kind: event.KIND_GIT_PR, CreatedAt: 1, Tags: [][]string{{"a", "30617:" + owner + ":repo"}, {"c", tip}, {"clone", server.URL + "/remote.git"}}}
	if err := event.Sign(&pr, secret); err != nil {
		t.Fatal(err)
	}
	if err := g.RepairPullRequestObjects(context.Background(), hosted, PullRequestRepair{Root: pr}); err == nil || !strings.Contains(err.Error(), "object graph") {
		t.Fatalf("expected incomplete object graph rejection, got %v", err)
	}
	if g.objectsPresent(context.Background(), hosted, []string{tip}) {
		t.Fatal("rejected objects reached the hosted repository")
	}
	gitDir := g.repoPath(hosted)
	if _, err := os.Stat(filepath.Join(gitDir, "shallow")); !os.IsNotExist(err) {
		t.Fatalf("hosted shallow boundary changed: %v", err)
	}
	if out := prGitOutput(t, "--git-dir", gitDir, "for-each-ref", "refs/nostr/"); strings.TrimSpace(out) != "" {
		t.Fatalf("rejected PR installed refs: %s", out)
	}
	// Once the existing base is hosted, the exact same PR must succeed even
	// though its complete ancestry extends beyond the remote depth window.
	prGitRun(t, "--git-dir", gitDir, "fetch", work, base+":refs/heads/main")
	if err := g.RepairPullRequestObjects(context.Background(), hosted, PullRequestRepair{Root: pr}); err != nil {
		t.Fatalf("PR with hosted base failed: %v", err)
	}
	if got := strings.TrimSpace(prGitOutput(t, "--git-dir", gitDir, "rev-parse", "refs/nostr/"+pr.ID)); got != tip {
		t.Fatalf("PR tip = %s, want %s", got, tip)
	}
	prGitRun(t, "--git-dir", gitDir, "fsck", "--full", "--no-dangling")
	if _, err := os.Stat(filepath.Join(gitDir, "shallow")); !os.IsNotExist(err) {
		t.Fatalf("successful repair made host shallow: %v", err)
	}
}

func TestPrivatePolicyDoesNotContactUnconfiguredRepairSource(t *testing.T) {
	var requests int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	root := t.TempDir()
	cert, err := x509.ParseCertificate(server.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", ca)
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	p := policy.Defaults("")
	p.Reads = "members"
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git"), Policy: func() policy.Policy { return p }, AllowPrivateRelays: true})
	if err != nil {
		t.Fatal(err)
	}
	repo := Repository{Owner: strings.Repeat("a", 64), Identifier: "private", Clone: []string{server.URL + "/repo.git"}, Refs: map[string]string{"refs/heads/main": strings.Repeat("b", 40)}}
	err = g.FetchMissing(context.Background(), repo, repo.Clone, repo.Refs)
	if err == nil {
		t.Fatal("private-policy repair unexpectedly succeeded")
	}
	if requests != 0 {
		t.Fatalf("private-policy repair contacted unconfigured source %d time(s)", requests)
	}
}

func TestGRASPTickResumesAfterCanceledRepository(t *testing.T) {
	p := policy.Policy{Features: policy.Features{Grasp: true, Grasp02: true}}
	owner := strings.Repeat("a", 64)
	first := Repository{Owner: owner, Identifier: "a", Refs: map[string]string{}}
	second := Repository{Owner: owner, Identifier: "b", Refs: map[string]string{}}
	var calls []string
	var cancel context.CancelFunc
	g := &GitRelay{
		root:   t.TempDir(),
		policy: func() policy.Policy { return p },
		repos:  map[string]Repository{key(first.Owner, first.Identifier): first, key(second.Owner, second.Identifier): second},
		gitSync: func(ctx context.Context, repo Repository) error {
			calls = append(calls, repo.Identifier)
			if len(calls) == 1 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	ctx, done := context.WithCancel(context.Background())
	cancel = done
	if err := g.GRASPService().Tick(ctx); err == nil {
		t.Fatal("canceled GRASP tick unexpectedly succeeded")
	}
	status, err := g.GRASPService().SyncStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := status.Repositories[key(second.Owner, second.Identifier)]; ok {
		t.Fatalf("unattempted repository received a status: %#v", status.Repositories)
	}
	if err := g.GRASPService().Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err = g.GRASPService().SyncStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Repositories[key(second.Owner, second.Identifier)].SucceededAt == 0 {
		t.Fatalf("resumed repository was not attempted: %#v", status.Repositories)
	}
	if len(calls) != 2 || calls[1] != "b" {
		t.Fatalf("resume order = %v, want [a b]", calls)
	}
}

func prGitRun(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func prGitOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

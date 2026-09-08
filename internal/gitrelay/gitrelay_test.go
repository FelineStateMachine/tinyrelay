package gitrelay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestParsePathAndNpub(t *testing.T) {
	if _, _, _, ok := parsePath("/not-a-repo"); ok {
		t.Fatal("accepted malformed repository path")
	}
	// This is the bech32 encoding of 32 zero bytes.
	npub := "npub1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"
	if _, err := decodeNPub(npub); err == nil {
		t.Fatal("accepted an invalid checksum")
	}
}

func TestPublishStagesRepositoryAndSignedStateHook(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git")})
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	announcement := event.Event{Kind: 30617, CreatedAt: 100, Tags: [][]string{{"d", "demo"}, {"clone", "https://relay.example/" + owner + "/demo.git"}}}
	if err := event.Sign(&announcement, secret); err != nil {
		t.Fatal(err)
	}
	if err := g.Publish(context.Background(), announcement); err != nil {
		t.Fatal(err)
	}
	state := event.Event{Kind: 30618, CreatedAt: 101, Tags: [][]string{{"d", "demo"}, {"HEAD", "ref: refs/heads/main"}, {"refs/heads/main", strings.Repeat("a", 40)}}}
	if err := event.Sign(&state, secret); err != nil {
		t.Fatal(err)
	}
	if err := g.Publish(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "git", owner, "demo.git")
	if _, err := os.Stat(filepath.Join(repo, "HEAD")); err != nil {
		t.Fatalf("bare repository missing: %v", err)
	}
	hook, err := os.ReadFile(filepath.Join(repo, "hooks", "pre-receive"))
	if err != nil || !strings.Contains(string(hook), "tinyrelay.refs") {
		t.Fatalf("state hook missing: %v", err)
	}
	refs, err := os.ReadFile(filepath.Join(repo, "tinyrelay.pending"))
	if err != nil || !strings.Contains(string(refs), "refs/heads/main "+strings.Repeat("a", 40)) {
		t.Fatalf("signed state was not staged: %v", err)
	}
}

func TestStateHEADSelectsNonMainBranch(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git")})
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("0", 63) + "1"
	owner, _ := event.PublicKey(secret)
	announcement := event.Event{Kind: 30617, CreatedAt: 100, Tags: [][]string{{"d", "feature"}}}
	if err := event.Sign(&announcement, secret); err != nil {
		t.Fatal(err)
	}
	if err := g.Publish(context.Background(), announcement); err != nil {
		t.Fatal(err)
	}
	state := event.Event{Kind: 30618, CreatedAt: 101, Tags: [][]string{{"d", "feature"}, {"HEAD", "ref: refs/heads/feat/atlas"}}}
	if err := event.Sign(&state, secret); err != nil {
		t.Fatal(err)
	}
	if err := g.Publish(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "git", owner, "feature.git")
	out, err := exec.Command("git", "--git-dir", repo, "symbolic-ref", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "refs/heads/feat/atlas" {
		t.Fatalf("HEAD=%q", strings.TrimSpace(string(out)))
	}
}

func TestStateProjectsExistingObjectsIntoNativeRefs(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git")})
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("0", 63) + "1"
	owner, _ := event.PublicKey(secret)
	ann := event.Event{Kind: 30617, CreatedAt: 1, Tags: [][]string{{"d", "existing"}}}
	if err := event.Sign(&ann, secret); err != nil {
		t.Fatal(err)
	}
	if err := g.Publish(context.Background(), ann); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "git", owner, "existing.git")
	tree, err := exec.Command("git", "--git-dir", repo, "mktree").Output()
	if err != nil {
		t.Fatal(err)
	}
	commitCmd := exec.Command("git", "--git-dir", repo, "commit-tree", strings.TrimSpace(string(tree)), "-m", "one")
	commitCmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	commit, err := commitCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	oid := strings.TrimSpace(string(commit))
	state := event.Event{Kind: 30618, CreatedAt: 2, Tags: [][]string{{"d", "existing"}, {"HEAD", "ref: refs/heads/main"}, {"refs/heads/main", oid}}}
	if err := event.Sign(&state, secret); err != nil {
		t.Fatal(err)
	}
	if err := g.Publish(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "ls-remote", repo).CombinedOutput(); err != nil || !strings.Contains(string(out), oid+"\trefs/heads/main") {
		t.Fatalf("ls-remote did not advertise signed state: %s (%v)", out, err)
	}
	ref, err := exec.Command("git", "--git-dir", repo, "rev-parse", "refs/heads/main").Output()
	if err != nil || strings.TrimSpace(string(ref)) != oid {
		t.Fatalf("native ref not projected: %s (%v)", ref, err)
	}

	state2 := event.Event{Kind: 30618, CreatedAt: 3, Tags: [][]string{{"d", "existing"}, {"HEAD", "ref: refs/heads/next"}, {"refs/heads/next", oid}}}
	if err := event.Sign(&state2, secret); err != nil {
		t.Fatal(err)
	}
	if err := g.Publish(context.Background(), state2); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "ls-remote", repo).CombinedOutput(); err != nil || strings.Contains(string(out), "refs/heads/main") || !strings.Contains(string(out), oid+"\trefs/heads/next") {
		t.Fatalf("ls-remote after state replacement = %s (%v)", out, err)
	}
	refs, err := exec.Command("git", "--git-dir", repo, "for-each-ref", "--format=%(refname)", "refs/heads").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(refs)); got != "refs/heads/next" {
		t.Fatalf("native refs after replacement = %q", got)
	}
	head, err := exec.Command("git", "--git-dir", repo, "symbolic-ref", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != "refs/heads/next" {
		t.Fatalf("HEAD after replacement = %q (%v)", strings.TrimSpace(string(head)), err)
	}
}

func TestPendingPromotionRemovesRefsDroppedByState(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git")})
	if err != nil {
		t.Fatal(err)
	}
	r := Repository{Owner: strings.Repeat("a", 64), Identifier: "pending"}
	if err := g.ensureRepo(r); err != nil {
		t.Fatal(err)
	}
	tree, err := exec.Command("git", "--git-dir", g.repoPath(r), "mktree").Output()
	if err != nil {
		t.Fatal(err)
	}
	objects := make([]string, 2)
	for i, message := range []string{"old", "new"} {
		cmd := exec.Command("git", "--git-dir", g.repoPath(r), "commit-tree", strings.TrimSpace(string(tree)), "-m", message)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		objects[i] = strings.TrimSpace(string(out))
	}
	old := Repository{Owner: r.Owner, Identifier: r.Identifier, Refs: map[string]string{"refs/heads/old": objects[0]}}
	if err := g.writeState(old); err != nil {
		t.Fatal(err)
	}
	next := Repository{Owner: r.Owner, Identifier: r.Identifier, Refs: map[string]string{"refs/heads/new": objects[1]}}
	if err := g.writePendingState(next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(g.pendingStatePath(next), g.statePath(next)); err != nil {
		t.Fatal(err)
	}
	if err := g.writeState(next); err != nil {
		t.Fatal(err)
	}
	refs, err := exec.Command("git", "--git-dir", g.repoPath(r), "for-each-ref", "--format=%(refname)", "refs/heads").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(refs)); got != "refs/heads/new" {
		t.Fatalf("native refs after pending promotion = %q", got)
	}
}

func TestStaleStateWorkerCannotRewindHEAD(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git")})
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("0", 63) + "1"
	owner, _ := event.PublicKey(secret)
	ann := event.Event{Kind: 30617, CreatedAt: 1, Tags: [][]string{{"d", "rewind"}}}
	if err := event.Sign(&ann, secret); err != nil {
		t.Fatal(err)
	}
	if err := g.Publish(context.Background(), ann); err != nil {
		t.Fatal(err)
	}
	first := event.Event{Kind: 30618, CreatedAt: 2, Tags: [][]string{{"d", "rewind"}, {"HEAD", "ref: refs/heads/first"}}}
	second := event.Event{Kind: 30618, CreatedAt: 3, Tags: [][]string{{"d", "rewind"}, {"HEAD", "ref: refs/heads/second"}}}
	for _, item := range []*event.Event{&first, &second} {
		if err := event.Sign(item, secret); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Save(context.Background(), *item, storage.SaveOptions{Now: item.CreatedAt}); err != nil {
			t.Fatal(err)
		}
	}
	r1, err := g.parseRepository(first)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := g.parseRepository(second)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.CommitAfterStoreNoNotify(context.Background(), second, r2); err != nil {
		t.Fatal(err)
	}
	if err := g.CommitAfterStoreNoNotify(context.Background(), first, r1); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "git", owner, "rewind.git")
	head, err := exec.Command("git", "--git-dir", repo, "symbolic-ref", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(head)) != "refs/heads/second" {
		t.Fatalf("stale worker rewound HEAD to %q", strings.TrimSpace(string(head)))
	}
}

func TestObjectsPresentBatchesManyRefsAndDetectsMissing(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git")})
	if err != nil {
		t.Fatal(err)
	}
	r := Repository{Owner: strings.Repeat("a", 64), Identifier: "many"}
	if err := g.ensureRepo(r); err != nil {
		t.Fatal(err)
	}
	var objects []string
	for i := 0; i < 128; i++ {
		cmd := exec.Command("git", "--git-dir", g.repoPath(r), "hash-object", "-w", "--stdin")
		cmd.Stdin = strings.NewReader(strings.Repeat("object", i+1))
		id, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		objects = append(objects, strings.TrimSpace(string(id)))
	}
	if !g.objectsPresent(context.Background(), r, objects) {
		t.Fatal("batched object check rejected present objects")
	}
	objects = append(objects, strings.Repeat("f", 40))
	if g.objectsPresent(context.Background(), r, objects) {
		t.Fatal("batched object check accepted a missing object")
	}
}

func TestAdmitSourceRejectsUnsafeEndpoints(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git"), ServiceURL: "https://relay.example"})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"http://example.com/repo.git", "https://localhost/repo.git", "https://127.0.0.1/repo.git", "https://user@example.com/repo.git", "https://relay.example/"} {
		if _, err := g.AdmitSource(raw); err == nil {
			t.Errorf("accepted unsafe source %q", raw)
		}
	}
	got, err := g.AdmitSource("https://git.example/repo.git/")
	if err != nil || got != "https://git.example/repo.git" {
		t.Fatalf("admitted source = %q, %v", got, err)
	}
}

func TestAlternativePRPublicationUsesSeparateRepository(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git"), EnableGRASP06: true})
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	object := "pending PR object"
	hashCommand := exec.Command("git", "hash-object", "--stdin")
	hashCommand.Stdin = strings.NewReader(object)
	hashOutput, err := hashCommand.Output()
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.TrimSpace(string(hashOutput))
	pr := event.Event{Kind: 1617, CreatedAt: 100, Tags: [][]string{{"d", "demo"}, {"c", hash}}}
	if err := event.Sign(&pr, secret); err != nil {
		t.Fatal(err)
	}
	if err := g.Publish(context.Background(), pr); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "git", "prs", owner, "demo.git", "HEAD")); err != nil {
		t.Fatal(err)
	}
	prRepo := filepath.Join(root, "git", "prs", owner, "demo.git")
	if _, err := os.Stat(filepath.Join(prRepo, "tinyrelay.pending")); err != nil {
		t.Fatalf("missing-object PR was not retained in pending state: %v", err)
	}
	if refs, err := exec.Command("git", "--git-dir", prRepo, "for-each-ref", "refs/nostr").Output(); err != nil {
		t.Fatal(err)
	} else if len(strings.TrimSpace(string(refs))) != 0 {
		t.Fatalf("pending PR ref became visible: %s", refs)
	}
	if err := g.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !g.IsPending(pr.ID) {
		t.Fatal("pending PR was not recovered after reload")
	}
	storeObject := exec.Command("git", "--git-dir", prRepo, "hash-object", "-w", "--stdin")
	storeObject.Stdin = strings.NewReader(object)
	if out, err := storeObject.CombinedOutput(); err != nil {
		t.Fatalf("store PR object: %v: %s", err, out)
	}
	ref := "refs/nostr/" + pr.ID
	if out, err := exec.Command("git", "--git-dir", prRepo, "update-ref", ref, hash).CombinedOutput(); err != nil {
		t.Fatalf("install PR ref: %v: %s", err, out)
	}
	hook := filepath.Join(prRepo, "hooks", "post-receive")
	cmd := exec.Command("sh", hook)
	cmd.Dir = prRepo
	cmd.Env = append(os.Environ(), "GIT_DIR="+prRepo)
	cmd.Stdin = strings.NewReader("0 " + hash + " " + ref + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("post-receive promotion: %v: %s", err, out)
	}
	got, err := exec.Command("git", "--git-dir", prRepo, "rev-parse", ref).Output()
	if err != nil || strings.TrimSpace(string(got)) != hash {
		t.Fatalf("promoted PR ref = %q, %v", got, err)
	}
	content, err := exec.Command("git", "--git-dir", prRepo, "cat-file", "blob", hash).Output()
	if err != nil || strings.TrimSpace(string(content)) != object {
		t.Fatalf("promoted PR object content = %q, %v", content, err)
	}
	if len(g.Capabilities()) != 2 || g.Capabilities()[1] != "GRASP-06" {
		t.Fatalf("capabilities = %#v", g.Capabilities())
	}
}

func TestJournalRecoveryCompletesStateFile(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git")})
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	state := event.Event{Kind: 30618, CreatedAt: 1, Tags: [][]string{{"d", "demo"}, {"refs/heads/main", strings.Repeat("c", 40)}}}
	if err := event.Sign(&state, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), state, storage.SaveOptions{Now: 1}); err != nil {
		t.Fatal(err)
	}
	r := Repository{Owner: owner, Identifier: "demo", Refs: map[string]string{"refs/heads/main": strings.Repeat("c", 40)}}
	jr := journalRecord{Repository: key(r.Owner, r.Identifier), EventID: state.ID, Kind: 30618, Refs: r.Refs}
	if err := g.writeJournal(r, jr); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Store: store, Root: filepath.Join(root, "git")}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "git", r.Owner, "demo.git", "tinyrelay.pending"))
	if err != nil || !strings.Contains(string(b), "refs/heads/main "+strings.Repeat("c", 40)) {
		t.Fatalf("recovery did not restore state: %q %v", b, err)
	}
}

func TestNewRebuildsRepositoriesAfterRestart(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secret := strings.Repeat("0", 63) + "1"
	owner, _ := event.PublicKey(secret)
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git")})
	if err != nil {
		t.Fatal(err)
	}
	announcement := event.Event{Kind: 30617, CreatedAt: 1, Tags: [][]string{{"d", "restart"}}}
	if err := event.Sign(&announcement, secret); err != nil {
		t.Fatal(err)
	}
	if err := g.Publish(context.Background(), announcement); err != nil {
		t.Fatal(err)
	}
	repoPath := filepath.Join(root, "git", owner, "restart.git")
	blobCmd := exec.Command("git", "--git-dir", repoPath, "hash-object", "-w", "--stdin")
	blobCmd.Stdin = strings.NewReader("restart content\n")
	content, err := blobCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	treeCmd := exec.Command("git", "--git-dir", repoPath, "mktree")
	treeCmd.Stdin = strings.NewReader("100644 blob " + strings.TrimSpace(string(content)) + "\tREADME\n")
	tree, err := treeCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	commitCmd := exec.Command("git", "--git-dir", repoPath, "commit-tree", strings.TrimSpace(string(tree)), "-m", "restart")
	commitCmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	commit, err := commitCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	state := event.Event{Kind: 30618, CreatedAt: 2, Tags: [][]string{{"d", "restart"}, {"HEAD", "ref: refs/heads/main"}, {"refs/heads/main", strings.TrimSpace(string(commit))}}}
	if err := event.Sign(&state, secret); err != nil {
		t.Fatal(err)
	}
	if err := g.Publish(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "--git-dir", repoPath, "update-ref", "refs/heads/unannounced", strings.TrimSpace(string(commit))).CombinedOutput(); err != nil {
		t.Fatalf("stage unannounced ref: %v %s", err, out)
	}
	restarted, err := New(Config{Store: store, Root: filepath.Join(root, "git")})
	if err != nil {
		t.Fatal(err)
	}
	r, err := restarted.lookup(owner, "restart")
	if err != nil {
		t.Fatalf("repository missing after New restart: %v", err)
	}
	if r.Refs["refs/heads/main"] != strings.TrimSpace(string(commit)) || r.Head != "ref: refs/heads/main" {
		t.Fatalf("signed state missing after restart: %#v", r)
	}
	if _, visible := r.Refs["refs/heads/unannounced"]; visible {
		t.Fatal("restart exposed a ref outside published signed authority")
	}
	page, err := restarted.Browse(context.Background(), BrowseRequest{Owner: owner, Repo: "restart", View: "file", Path: "README"})
	if err != nil || page.Content != "restart content\n" {
		t.Fatalf("browser content after restart = %q, %v", page.Content, err)
	}
	readme, err := exec.Command("git", "--git-dir", repoPath, "show", "refs/heads/main:README").Output()
	if err != nil || string(readme) != "restart content\n" {
		t.Fatalf("restarted content = %q, %v", readme, err)
	}
}

func TestGRASPServiceTickPersistsProgressAndRepairOrder(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	called := 0
	p := policy.Defaults("")
	p.Features.Grasp = true
	p.Features.Grasp02 = true
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git"), Policy: func() policy.Policy { return p }, GitSync: func(context.Context, Repository) error { called++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	r := Repository{Owner: strings.Repeat("d", 64), Identifier: "repo", Clone: []string{"https://clone.example/repo.git"}, Relays: []string{"https://relay.example"}}
	g.mu.Lock()
	g.repos[key(r.Owner, r.Identifier)] = r
	g.mu.Unlock()
	if err := g.GRASPService().Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatalf("Git sync callback count = %d", called)
	}
	if _, err := os.Stat(filepath.Join(root, "git", ".tinyrelay", "grasp-progress.json")); err != nil {
		t.Fatal(err)
	}
	sources := RepairSources(r)
	if len(sources) != 2 || sources[0] != r.Clone[0] || sources[1] != r.Relays[0] {
		t.Fatalf("repair sources = %#v", sources)
	}
}

func TestGRASPServiceTickContinuesAfterRepositoryFailure(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p := policy.Defaults("")
	p.Features.Grasp = true
	p.Features.Grasp02 = true
	var synced []string
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git"), Policy: func() policy.Policy { return p }, GitSync: func(_ context.Context, r Repository) error {
		synced = append(synced, r.Identifier)
		if r.Identifier == "first" {
			return errors.New("temporary source failure")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		r := Repository{Owner: strings.Repeat("e", 64), Identifier: id, Clone: []string{"https://clone.example/" + id}}
		g.mu.Lock()
		g.repos[key(r.Owner, r.Identifier)] = r
		g.mu.Unlock()
	}
	if err := g.GRASPService().Tick(context.Background()); err == nil {
		t.Fatal("Tick hid repository failure")
	}
	if len(synced) != 2 || synced[0] != "first" || synced[1] != "second" {
		t.Fatalf("sync order/calls = %#v", synced)
	}
	b, err := os.ReadFile(filepath.Join(root, "git", ".tinyrelay", "grasp-progress.json"))
	if err != nil {
		t.Fatal(err)
	}
	var progress tickProgress
	if err := json.Unmarshal(b, &progress); err != nil {
		t.Fatal(err)
	}
	if progress.Repos[key(strings.Repeat("e", 64), "first")].Error == "" || progress.Repos[key(strings.Repeat("e", 64), "second")].SucceededAt == 0 {
		t.Fatalf("per-repository progress = %#v", progress.Repos)
	}
	status, err := g.GRASPService().SyncStatus(context.Background())
	if err != nil || status.Repositories[key(strings.Repeat("e", 64), "second")].SucceededAt == 0 {
		t.Fatalf("SyncStatus = %#v, %v", status, err)
	}
	g.mu.Lock()
	changed := g.repos[key(strings.Repeat("e", 64), "first")]
	changed.EventID = "new-state"
	g.repos[key(changed.Owner, changed.Identifier)] = changed
	g.mu.Unlock()
	if err := g.GRASPService().Tick(context.Background()); err == nil {
		t.Fatal("changed repository state did not bypass backoff")
	}
	if len(synced) != 3 {
		t.Fatalf("changed state sync calls = %#v", synced)
	}
	if err := g.GRASPService().Tick(context.Background()); err != nil {
		t.Fatalf("second tick ignored durable backoff: %v", err)
	}
	status, err = g.GRASPService().SyncStatus(context.Background())
	if err != nil || status.Repositories[key(strings.Repeat("e", 64), "first")].Attempts != 2 || status.Repositories[key(strings.Repeat("e", 64), "first")].NextAt <= time.Now().Unix() {
		t.Fatalf("retry progress = %#v, %v", status, err)
	}
	p.Features.Grasp03 = true
	if err := g.GRASPService().Tick(context.Background()); err == nil {
		t.Fatal("policy profile change did not invalidate backoff")
	}
	if len(synced) != 5 {
		t.Fatalf("policy change sync calls = %#v", synced)
	}
}

func TestValidateAndCommitAfterStoreAvoidDuplicateSave(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git")})
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("0", 63) + "1"
	e := event.Event{Kind: 30617, CreatedAt: 1, Tags: [][]string{{"d", "manual"}}}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	r, err := g.Validate(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), e, storage.SaveOptions{Now: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := g.CommitAfterStore(context.Background(), e, r); err != nil {
		t.Fatal(err)
	}
	q, err := store.Query(context.Background(), event.Filter{IDs: []string{e.ID}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 10})
	if err != nil || len(q.Events) != 1 {
		t.Fatalf("stored events = %d, err=%v", len(q.Events), err)
	}
}

func TestSmartHTTPServesBareRepository(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git")})
	if err != nil {
		t.Fatal(err)
	}
	owner := "3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d"
	r := Repository{Owner: owner, Identifier: "http", Refs: map[string]string{}}
	g.mu.Lock()
	g.repos[key(owner, "http")] = r
	g.mu.Unlock()
	if err := g.ensureRepo(r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "git", owner, "http.git", "HEAD")); err != nil {
		t.Fatalf("repo: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/npub/npub180cvv07tjdrrgpa0j7j7tmnyl2yr6yr7l8j4s3evf6u64th6gkwsyjh6w6/http.git/info/refs?service=git-upload-pack", nil)
	if got, err := decodeNPub("npub180cvv07tjdrrgpa0j7j7tmnyl2yr6yr7l8j4s3evf6u64th6gkwsyjh6w6"); err != nil {
		t.Fatalf("known npub: %v", err)
	} else if got != owner {
		t.Fatalf("known npub owner: %s", got)
	}
	po, pi, ps, pa, pok := parsePathMode(req.URL.Path)
	if !pok || po != owner || pi != "http" || ps != "/info/refs" || pa {
		t.Fatalf("parsed path %s: %q %q %q %v %v", req.URL.Path, po, pi, ps, pa, pok)
	}
	res := httptest.NewRecorder()
	g.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("smart HTTP status = %d: %s", res.Code, res.Body.String())
	}
	body, _ := io.ReadAll(res.Result().Body)
	if len(body) == 0 {
		t.Fatal("smart HTTP returned an empty advertisement")
	}
	// gitworkshop.dev and other browser clients require these capabilities.
	for _, capability := range []string{" filter", " allow-tip-sha1-in-want", " allow-reachable-sha1-in-want", " object-format=sha1"} {
		if !strings.Contains(string(body), capability) {
			t.Errorf("advertisement lacks %q: %s", strings.TrimSpace(capability), body)
		}
	}
}

func TestPendingStatePromotesAfterAuthorizedGitPush(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g, err := New(Config{Store: store, Root: filepath.Join(root, "git")})
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("0", 63) + "1"
	owner, _ := event.PublicKey(secret)
	ann := event.Event{Kind: 30617, CreatedAt: 1, Tags: [][]string{{"d", "push"}}}
	if err := event.Sign(&ann, secret); err != nil {
		t.Fatal(err)
	}
	if err := g.Publish(context.Background(), ann); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "source.git")
	if out, err := exec.Command("git", "init", "--bare", src).CombinedOutput(); err != nil {
		t.Fatalf("init: %s", out)
	}
	work := filepath.Join(root, "work")
	if out, err := exec.Command("git", "clone", src, work).CombinedOutput(); err != nil {
		t.Fatalf("clone: %s", out)
	}
	for _, args := range [][]string{{"-C", work, "config", "user.email", "test@example.com"}, {"-C", work, "config", "user.name", "Test"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("config: %s", out)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("pending\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", work, "add", "README").CombinedOutput(); err != nil {
		t.Fatalf("add: %s", out)
	}
	if out, err := exec.Command("git", "-C", work, "commit", "-m", "pending").CombinedOutput(); err != nil {
		t.Fatalf("commit: %s", out)
	}
	if out, err := exec.Command("git", "-C", work, "tag", "v1").CombinedOutput(); err != nil {
		t.Fatalf("tag: %s", out)
	}
	shaBytes, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaBytes))
	tagBytes, err := exec.Command("git", "-C", work, "rev-parse", "refs/tags/v1").Output()
	if err != nil {
		t.Fatal(err)
	}
	tagOID := strings.TrimSpace(string(tagBytes))
	state := event.Event{Kind: 30618, CreatedAt: 2, Tags: [][]string{{"d", "push"}, {"HEAD", "ref: refs/heads/main"}, {"refs/heads/main", sha}, {"refs/tags/v1", tagOID}}}
	if err := event.Sign(&state, secret); err != nil {
		t.Fatal(err)
	}
	if err := g.Publish(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "git", owner, "push.git")
	if _, err := os.Stat(filepath.Join(target, "tinyrelay.pending")); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", work, "push", target, "HEAD:refs/heads/main", "refs/tags/v1:refs/tags/v1").CombinedOutput(); err != nil {
		t.Fatalf("push: %s", out)
	}
	if _, err := os.Stat(filepath.Join(target, "tinyrelay.refs")); err != nil {
		t.Fatalf("pending state was not promoted: %v", err)
	}
}

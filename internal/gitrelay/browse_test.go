package gitrelay

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func browseFixture(t *testing.T) (*GitRelay, Repository, string) {
	t.Helper()
	root := t.TempDir()
	r := Repository{Owner: strings.Repeat("a", 64), Identifier: "browse", Head: "refs/heads/main"}
	g := &GitRelay{root: root, repos: map[string]Repository{}}
	dir := g.repoPath(r)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Browser Test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Browser Test", "GIT_COMMITTER_EMAIL=test@example.test")
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	run("init", "-b", "main")
	for name, body := range map[string]string{"source.go": "package main\n// <script>alert(1)</script>\n", "[literal]*.txt": "literal path", "large.txt": strings.Repeat("x", 300*1024), "binary.bin": "a\x00b"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-m", "first source")
	first := run("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "source.go"), []byte("package main\n// second\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("commit", "-am", "second source")
	r.Refs = map[string]string{"refs/heads/main": run("rev-parse", "HEAD")}
	g.repos[key(r.Owner, r.Identifier)] = r
	// Browsing normally uses a bare repository; this fixture switches --git-dir to .git.
	g.root = filepath.Join(root, "bare")
	if err := os.MkdirAll(filepath.Dir(g.repoPath(r)), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "clone", "--bare", dir, g.repoPath(r))
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bare clone: %v %s", err, b)
	}
	return g, r, first
}

func TestBrowseRepositorySourceAndHistory(t *testing.T) {
	g, r, first := browseFixture(t)
	ctx := context.Background()
	q := BrowseRequest{Owner: r.Owner, Repo: r.Identifier}
	page, err := g.Browse(ctx, q)
	if err != nil || len(page.Entries) != 4 {
		t.Fatalf("tree: %+v %v", page, err)
	}
	q.View, q.Path, q.Ref = "file", "[literal]*.txt", first
	page, err = g.Browse(ctx, q)
	if err != nil || page.Content != "literal path" {
		t.Fatalf("literal: %+v %v", page, err)
	}
	q.Path = "source.go"
	page, err = g.Browse(ctx, q)
	if err != nil || !strings.Contains(page.Content, "<script>") {
		t.Fatalf("historical source: %+v %v", page, err)
	}
	q.View, q.Ref, q.Path, q.Limit = "history", "main", "", 1
	page, err = g.Browse(ctx, q)
	if err != nil || len(page.Commits) != 1 || page.NextOffset != 1 {
		t.Fatalf("history: %+v %v", page, err)
	}
	q.View = "commit"
	page, err = g.Browse(ctx, q)
	if err != nil || !strings.Contains(page.Diff, "+// second") {
		t.Fatalf("diff: %+v %v", page, err)
	}
}

func TestFetchMissingUsesCompleteLocalObjects(t *testing.T) {
	g, repo, _ := browseFixture(t)
	expected := map[string]string{"refs/heads/repaired": repo.Refs["refs/heads/main"]}
	if err := g.FetchMissing(context.Background(), repo, nil, expected); err != nil {
		t.Fatalf("complete local inventory should not need a network source: %v", err)
	}
	native, err := g.nativeRefs(repo)
	if err != nil || native["refs/heads/repaired"] != expected["refs/heads/repaired"] {
		t.Fatalf("locally available objects were not projected to expected refs: %v %v", native, err)
	}
}

func TestBrowseRejectsUnsafeOrUnpublishedObjects(t *testing.T) {
	g, r, _ := browseFixture(t)
	cmd := exec.Command("git", "--git-dir", g.repoPath(r), "hash-object", "-w", "--stdin")
	cmd.Stdin = strings.NewReader("unpublished secret")
	id, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []BrowseRequest{
		{Owner: r.Owner, Repo: r.Identifier, Ref: "--output=/tmp/nope"},
		{Owner: r.Owner, Repo: r.Identifier, Ref: strings.TrimSpace(string(id))},
		{Owner: r.Owner, Repo: r.Identifier, Path: "../config"},
		{Owner: r.Owner, Repo: r.Identifier, Path: "/etc/passwd"},
	} {
		if _, err := g.Browse(context.Background(), q); err == nil {
			t.Fatalf("accepted unsafe request: %+v", q)
		}
	}
}

func TestBrowseFilePreviewIsBoundedAndBinaryAware(t *testing.T) {
	g, r, _ := browseFixture(t)
	for _, name := range []string{"large.txt", "binary.bin"} {
		page, err := g.Browse(context.Background(), BrowseRequest{Owner: r.Owner, Repo: r.Identifier, View: "file", Path: name})
		if err != nil {
			t.Fatal(err)
		}
		if name == "large.txt" && (!page.Truncated || len(page.Content) > 256*1024) {
			t.Fatal("unbounded preview")
		}
		if name == "binary.bin" && (!page.Binary || page.Content != "") {
			t.Fatal("binary shown as source")
		}
	}
}

func TestBrowseHiddenCommitAndFullBinaryDownload(t *testing.T) {
	g, r, _ := browseFixture(t)
	cmd := g.browseCommand(context.Background(), r, "rev-parse", "HEAD^{tree}")
	tree, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd = g.browseCommand(context.Background(), r, "commit-tree", strings.TrimSpace(string(tree)))
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.test")
	cmd.Stdin = strings.NewReader("unpublished commit")
	hidden, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	oid := strings.TrimSpace(string(hidden))
	if err := g.browseCommand(context.Background(), r, "update-ref", "refs/tinyrelay/pending/secret", oid).Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Browse(context.Background(), BrowseRequest{Owner: r.Owner, Repo: r.Identifier, Ref: oid}); err == nil {
		t.Fatal("unpublished commit exposed")
	}
	var result bytes.Buffer
	if err := g.WriteSource(context.Background(), BrowseRequest{Owner: r.Owner, Repo: r.Identifier, Path: "large.txt"}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Len() != 300*1024 {
		t.Fatalf("download truncated: %d", result.Len())
	}
}

func TestBrowseTreePaginationHasNoMissingEntries(t *testing.T) {
	g, r, _ := browseFixture(t)
	names := map[string]bool{}
	q := BrowseRequest{Owner: r.Owner, Repo: r.Identifier, Limit: 1}
	for {
		page, err := g.Browse(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range page.Entries {
			if names[entry.Name] {
				t.Fatal("duplicate entry")
			}
			names[entry.Name] = true
		}
		if page.NextOffset < 0 {
			break
		}
		q.Offset = page.NextOffset
	}
	if len(names) != 4 {
		t.Fatalf("missing entries: %v", names)
	}
}

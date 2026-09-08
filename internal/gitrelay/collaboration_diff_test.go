package gitrelay

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestPRDiffUsesLocalCommitsAndReportsMetadata(t *testing.T) {
	g, r, base := browseFixture(t)
	tip := r.Refs["refs/heads/main"]
	got := g.PRDiff(context.Background(), r, base, tip)
	if got.Status != "ok" || got.Base != base || got.Tip != tip || got.MergeBase != base {
		t.Fatalf("result = %+v", got)
	}
	if !strings.Contains(got.Diff, "+// second") || got.Truncated {
		t.Fatalf("diff = %+v", got)
	}
}

func TestPRDiffDoesNotTurnMissingOrUnrelatedObjectsIntoEmptyDiff(t *testing.T) {
	g, r, base := browseFixture(t)
	tip := r.Refs["refs/heads/main"]
	missing := strings.Repeat("f", 40)
	if got := g.PRDiff(context.Background(), r, base, missing); got.Status != "diff_unavailable" || got.Reason == "" {
		t.Fatalf("missing tip result = %+v", got)
	}
	if got := g.PRDiff(context.Background(), r, missing, r.Refs["refs/heads/main"]); got.Status != "diff_unavailable" || got.Reason == "" {
		t.Fatalf("missing base result = %+v", got)
	}
	// The first commit in the fixture is not related to an independently
	// created root. The backend must disclose that it cannot establish a PR
	// base instead of presenting a whole-tree comparison as a normal diff.
	tree, err := exec.Command("git", "--git-dir", g.repoPath(r), "mktree").Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "--git-dir", g.repoPath(r), "commit-tree", strings.TrimSpace(string(tree)), "-m", "unrelated")
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.test")
	otherBytes, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	other := strings.TrimSpace(string(otherBytes))
	if got := g.PRDiff(context.Background(), r, other, base); got.Status != "diff_unavailable" {
		t.Fatalf("unrelated result = %+v", got)
	}
	// This base is an ancestor of the PR tip but belongs only to that PR;
	// it is not an ancestor of the repository target.
	treeBytes, err := exec.Command("git", "--git-dir", g.repoPath(r), "rev-parse", base+"^{tree}").Output()
	if err != nil {
		t.Fatal(err)
	}
	commit := func(parents ...string) string {
		args := []string{"--git-dir", g.repoPath(r), "commit-tree", strings.TrimSpace(string(treeBytes))}
		for _, parent := range parents {
			args = append(args, "-p", parent)
		}
		args = append(args, "-m", "PR-only")
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.test")
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	prBase := commit(base)
	prTip := commit(prBase)
	if got := g.PRDiff(context.Background(), r, prBase, prTip); got.Status != "diff_unavailable" {
		t.Fatalf("PR-only ancestor result = %+v (main tip %s)", got, tip)
	}
}

func TestPRDiffAcceptsBaseWhenTargetBranchDiverged(t *testing.T) {
	g, r, base := browseFixture(t)
	tip := r.Refs["refs/heads/main"]
	treeBytes, err := exec.Command("git", "--git-dir", g.repoPath(r), "rev-parse", base+"^{tree}").Output()
	if err != nil {
		t.Fatal(err)
	}
	commit := func(message string) string {
		cmd := exec.Command("git", "--git-dir", g.repoPath(r), "commit-tree", strings.TrimSpace(string(treeBytes)), "-p", base, "-m", message)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.test")
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	target := commit("target branch")
	if err := exec.Command("git", "--git-dir", g.repoPath(r), "update-ref", "refs/heads/target", target).Run(); err != nil {
		t.Fatal(err)
	}
	r.Head = "refs/heads/target"
	got := g.PRDiff(context.Background(), r, base, tip)
	if got.Status != "ok" || got.MergeBase != base {
		t.Fatalf("diverged target result = %+v", got)
	}
}

func TestPRDiffFindsMergeBaseFromRepositoryHead(t *testing.T) {
	g, r, base := browseFixture(t)
	tip := r.Refs["refs/heads/main"]
	r.Head = "refs/heads/main"
	got := g.PRDiff(context.Background(), r, "", tip)
	if got.Status != "ok" || got.MergeBase != tip || got.Base != tip {
		t.Fatalf("result = %+v (base fixture commit %s)", got, base)
	}
}

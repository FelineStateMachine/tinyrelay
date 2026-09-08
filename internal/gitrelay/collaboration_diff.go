package gitrelay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const collaborationDiffBytes = 512 * 1024
const collaborationDiffTimeout = 5 * time.Second

// CollaborationDiff is the result of rendering a pull request's local Git
// objects. Status is "ok" when Diff is authoritative and
// "diff_unavailable" when one or both commits are not present locally.
type CollaborationDiff struct {
	Status    string `json:"status"`
	Base      string `json:"base,omitempty"`
	Tip       string `json:"tip,omitempty"`
	MergeBase string `json:"merge_base,omitempty"`
	Diff      string `json:"diff,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// PRDiff renders a bounded diff from commits already present in the
// repository. It never fetches or resolves a remote ref. A supplied base is
// used only after it is verified as a local commit; when it is empty, Git's
// merge-base with the repository target is used.
func (g *GitRelay) PRDiff(ctx context.Context, r Repository, base, tip string) CollaborationDiff {
	ctx, cancel := context.WithTimeout(ctx, collaborationDiffTimeout)
	defer cancel()
	result := CollaborationDiff{Status: "diff_unavailable", Base: base, Tip: tip}
	if !isObjectID(base) && base != "" {
		result.Reason = "invalid base commit"
		return result
	}
	if !isObjectID(tip) {
		result.Reason = "invalid tip commit"
		return result
	}
	if err := g.localCommit(ctx, r, tip); err != nil {
		result.Reason = "tip commit is unavailable locally"
		return result
	}
	if base != "" {
		if err := g.localCommit(ctx, r, base); err != nil {
			result.Reason = "base commit is unavailable locally"
			return result
		}
	}
	target, err := g.targetCommit(ctx, r)
	if err != nil {
		result.Reason = "repository target commit is unavailable locally"
		return result
	}
	if base != "" {
		// A PR base is meaningful only when it is reachable from both sides of
		// the comparison. Checking the current target as well prevents an old
		// or malicious PR-only ancestor from being presented as the repository
		// diff base.
		if err := g.browseCommand(ctx, r, "merge-base", "--is-ancestor", base, tip).Run(); err != nil {
			result.Reason = "base is not an ancestor of tip"
			return result
		}
		if err := g.browseCommand(ctx, r, "merge-base", "--is-ancestor", base, target).Run(); err != nil {
			result.Reason = "base is not an ancestor of repository target"
			return result
		}
		result.MergeBase = base
	} else {
		b, err := g.browseCommand(ctx, r, "merge-base", "--", tip, target).Output()
		if err != nil || !isObjectID(strings.TrimSpace(string(b))) {
			result.Reason = "merge base is unavailable locally"
			return result
		}
		result.MergeBase = strings.TrimSpace(string(b))
		result.Base = result.MergeBase
	}

	b, truncated, err := readBrowseOutput(g.browseCommand(ctx, r, "diff", "--format=", "--no-ext-diff", "--no-textconv", "--no-renames", "--binary", result.MergeBase, tip, "--"), collaborationDiffBytes)
	if err != nil {
		result.Reason = "unable to render local diff"
		return result
	}
	result.Status = "ok"
	result.Diff = strings.ToValidUTF8(string(b), "�")
	result.Truncated = truncated
	return result
}

func (g *GitRelay) targetCommit(ctx context.Context, r Repository) (string, error) {
	target := strings.TrimPrefix(r.Head, "ref: ")
	if target == "" || !validBrowsePath(target) {
		return "", errors.New("missing repository target")
	}
	b, err := g.browseCommand(ctx, r, "rev-parse", "--verify", "--end-of-options", target+"^{commit}").Output()
	if err != nil {
		return "", err
	}
	target = strings.TrimSpace(string(b))
	if !isObjectID(target) || g.localCommit(ctx, r, target) != nil {
		return "", errors.New("target commit unavailable")
	}
	return target, nil
}

func (g *GitRelay) localCommit(ctx context.Context, r Repository, oid string) error {
	if !isObjectID(oid) {
		return errors.New("invalid object id")
	}
	b, err := g.browseCommand(ctx, r, "cat-file", "-t", oid).Output()
	if err != nil || strings.TrimSpace(string(b)) != "commit" {
		return fmt.Errorf("commit unavailable: %s", oid)
	}
	return nil
}

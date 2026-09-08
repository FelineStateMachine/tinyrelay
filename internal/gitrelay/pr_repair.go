package gitrelay

import (
	"context"
	"errors"
	"fmt"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// PullRequestRepair groups a PR with its later tip updates. The event store
// remains the authority for these events; this value only describes the Git
// objects that GRASP-02 should fetch for them.
type PullRequestRepair struct {
	Root    event.Event
	Updates []event.Event
}

// RepairPullRequestObjects fetches the commits named by a NIP-34 PR and its
// updates into the supplied hosted repository.
//
// Updates are accepted only from the PR author and must point to the root PR.
// This prevents an unrelated event from supplying a ref to a hosted PR.
func (g *GitRelay) RepairPullRequestObjects(ctx context.Context, hosted Repository, set PullRequestRepair) error {
	root, err := pullRequestTarget(set.Root, event.KIND_GIT_PR)
	if err != nil {
		return err
	}
	if !hasRepositoryCoordinate(set.Root, hosted) {
		return errors.New("blocked: pull request targets another repository")
	}
	// The hosted repository owns its path and access policy. PR events only
	// contribute temporary refs and clone sources.
	root = hosted
	root.Alternative = false
	sources := cloneURLs(set.Root)
	refs := map[string]string{"refs/nostr/" + set.Root.ID: event.Tag(set.Root, "c")}
	for _, update := range set.Updates {
		if err := validatePullRequestUpdate(set.Root, update); err != nil {
			return err
		}
		commit := event.Tag(update, "c")
		if commit == "" {
			continue
		}
		refs["refs/nostr/"+update.ID] = commit
		sources = append(sources, cloneURLs(update)...)
	}
	if len(sources) == 0 {
		return errors.New("GRASP-02: pull request has no clone source")
	}
	sources = uniqueStrings(sources)
	if err := g.FetchMissing(ctx, root, sources, refs); err != nil {
		return fmt.Errorf("GRASP-02 pull request repair: %w", err)
	}
	return nil
}

func hasRepositoryCoordinate(pr event.Event, repo Repository) bool {
	want := "30617:" + repo.Owner + ":" + repo.Identifier
	for _, tag := range pr.Tags {
		if len(tag) > 1 && (tag[0] == "a" || tag[0] == "A") && tag[1] == want {
			return true
		}
	}
	return false
}

func cloneURLs(e event.Event) []string {
	var out []string
	for _, tag := range e.Tags {
		if len(tag) > 1 && tag[0] == "clone" {
			out = append(out, tag[1:]...)
		}
	}
	return out
}

func pullRequestTarget(raw event.Event, kind int) (Repository, error) {
	if err := event.Validate(raw); err != nil {
		return Repository{}, err
	}
	if raw.Kind != kind {
		return Repository{}, fmt.Errorf("unsupported: event kind %d", raw.Kind)
	}
	commit := event.Tag(raw, "c")
	if !isObjectID(commit) {
		return Repository{}, errors.New("invalid: pull request commit is not a SHA-1")
	}
	return Repository{
		Owner:       raw.PubKey,
		Identifier:  raw.ID,
		EventID:     raw.ID,
		Alternative: true,
		Clone:       cloneURLs(raw),
		Refs:        map[string]string{"refs/nostr/" + raw.ID: commit},
	}, nil
}

func validatePullRequestUpdate(root, update event.Event) error {
	if err := event.Validate(update); err != nil {
		return err
	}
	if update.Kind != event.KIND_GIT_PR_UPDATE {
		return fmt.Errorf("unsupported: event kind %d is not a pull request update", update.Kind)
	}
	parent := event.Tag(update, "E")
	if parent == "" {
		parent = event.Tag(update, "e")
	}
	if parent != root.ID {
		return errors.New("blocked: pull request update does not reference its root")
	}
	declaredAuthor := event.Tag(update, "P")
	if declaredAuthor != root.PubKey {
		return errors.New("blocked: pull request update author tag does not match root")
	}
	if update.PubKey != root.PubKey {
		return errors.New("blocked: pull request update signer is not the PR author")
	}
	if !isObjectID(event.Tag(update, "c")) {
		return errors.New("invalid: pull request update commit is not a SHA-1")
	}
	return nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

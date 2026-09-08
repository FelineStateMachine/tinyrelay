package gitrelay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

const (
	// PR repair is fed by clone URLs carried in signed events. Keep its
	// temporary object graph finite while leaving ordinary branch repair
	// compatible with complete repositories.
	prRepairDepth    = 128
	prRepairMaxBytes = 256 << 20
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
	root.EventID = set.Root.ID
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
	if err := g.stagePullRequestObjects(ctx, root, sources, refs); err != nil {
		return fmt.Errorf("GRASP-02 pull request repair: %w", err)
	}
	return nil
}

// stagePullRequestObjects admits an untrusted PR clone into a quarantine bare
// repository first. Only after every expected object is present do we import
// the temporary refs into the hosted repository and install the signed refs.
// The quarantine is removed on return, and failed repairs never alter hosted
// refs. In particular, its shallow boundary is never copied to the host.
func (g *GitRelay) stagePullRequestObjects(ctx context.Context, hosted Repository, sources []string, expected map[string]string) error {
	if err := g.ensureRepo(hosted); err != nil {
		return err
	}
	// Keep the quarantine outside the hosted repository namespace. A clone URL
	// is signed input and can be processed concurrently with another repair.
	quarantineRoot, err := os.MkdirTemp("", "tinyrelay-pr-repair-")
	if err != nil {
		return fmt.Errorf("create PR repair quarantine: %w", err)
	}
	defer os.RemoveAll(quarantineRoot)
	stage := Repository{Owner: hosted.Owner, Identifier: "objects", Private: hosted.Private}
	g.mu.RLock()
	stageRelay := &GitRelay{
		root: quarantineRoot, policy: g.policy, serviceURL: g.serviceURL,
		allowPrivate: g.allowPrivate, privatePeers: append([]string(nil), g.privatePeers...),
		httpAuth: g.httpAuth, configured: make(map[string]struct{}),
	}
	g.mu.RUnlock()
	if err := stageRelay.ensureRepo(stage); err != nil {
		return fmt.Errorf("create PR repair quarantine: %w", err)
	}
	stagePath := stageRelay.repoPath(stage)
	// Let a PR build on objects already admitted to the hosted repository, but
	// never copy the quarantine's shallow boundary into that repository.
	alternates := filepath.Join(stagePath, "objects", "info", "alternates")
	hostObjects, err := filepath.Abs(filepath.Join(g.repoPath(hosted), "objects"))
	if err != nil {
		return fmt.Errorf("locate hosted PR base objects: %w", err)
	}
	if err := os.WriteFile(alternates, []byte(hostObjects+"\n"), 0600); err != nil {
		return fmt.Errorf("configure PR repair quarantine: %w", err)
	}
	if err := stageRelay.fetchMissing(ctx, stage, sources, expected, prRepairDepth, prRepairMaxBytes); err != nil {
		return err
	}
	if err := validatePRQuarantine(ctx, stagePath, nonEmptyObjectIDs(expected)); err != nil {
		return err
	}

	// Import into disposable refs. If a later refspec fails, clean them up and
	// leave the signed hosted refs untouched.
	cleanup := make([]string, 0, len(expected))
	defer func() {
		if len(cleanup) == 0 {
			return
		}
		var commands strings.Builder
		for _, ref := range cleanup {
			fmt.Fprintf(&commands, "delete %s\n", ref)
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cleanupCtx, "git", "--git-dir", g.repoPath(hosted), "update-ref", "--stdin")
		cmd.Stdin = strings.NewReader(commands.String())
		_ = cmd.Run()
	}()
	for ref, oid := range expected {
		if oid == "" {
			continue
		}
		staged := "refs/tinyrelay/pr-repair/" + filepath.Base(quarantineRoot) + "/" + safeRef(ref)
		cleanup = append(cleanup, staged)
		cmd := exec.CommandContext(ctx, "git", "--git-dir", g.repoPath(hosted), "fetch", "--no-tags", stagePath, "+"+oid+":"+staged)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("import PR repair object %s: %w: %s", oid, err, strings.TrimSpace(string(out)))
		}
	}
	updates := make([]string, 0, len(expected))
	for ref, oid := range expected {
		if oid != "" {
			updates = append(updates, "update "+ref+" "+oid+"\n")
		}
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir", g.repoPath(hosted), "update-ref", "--stdin")
	cmd.Stdin = strings.NewReader("start\n" + strings.Join(updates, "") + "prepare\ncommit\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("promote PR repair refs: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// validatePRQuarantine checks the complete object graph after removing the
// shallow marker. rev-list reports missing commits, trees and blobs through
// its --missing output; alternates make hosted base objects visible here.
func validatePRQuarantine(ctx context.Context, gitDir string, oids []string) error {
	if len(oids) == 0 {
		return errors.New("PR repair has no expected objects")
	}
	if err := os.Remove(filepath.Join(gitDir, "shallow")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove PR repair shallow boundary: %w", err)
	}
	args := append([]string{"--git-dir", gitDir, "rev-list", "--objects", "--missing=print"}, oids...)
	cmd := exec.CommandContext(ctx, "git", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("validate PR repair object graph: create pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("validate PR repair object graph: %w", err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "?") {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return fmt.Errorf("PR repair object graph is incomplete: %s", strings.TrimSpace(line))
		}
	}
	if err := scanner.Err(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("validate PR repair object graph: read output: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("validate PR repair object graph: %w", err)
	}
	// Ensure each advertised tip is an actual commit, rather than accepting a
	// tree or blob object under a commit-shaped event tag.
	for _, oid := range oids {
		check := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "cat-file", "-t", oid)
		kind, err := check.Output()
		if err != nil || strings.TrimSpace(string(kind)) != "commit" {
			return fmt.Errorf("PR repair tip %s is not a complete commit", oid)
		}
	}
	return nil
}

func nonEmptyObjectIDs(refs map[string]string) []string {
	ids := make([]string, 0, len(refs))
	for _, oid := range refs {
		if oid != "" {
			ids = append(ids, oid)
		}
	}
	return ids
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

package gitrelay

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func (g *GitRelay) repoPath(r Repository) string {
	if r.Alternative {
		return filepath.Join(g.root, "prs", r.Owner, r.Identifier+".git")
	}
	return filepath.Join(g.root, r.Owner, r.Identifier+".git")
}
func (g *GitRelay) statePath(r Repository) string {
	return filepath.Join(g.repoPath(r), "tinyrelay.refs")
}
func (g *GitRelay) pendingStatePath(r Repository) string {
	return filepath.Join(g.repoPath(r), "tinyrelay.pending")
}

func (g *GitRelay) pendingHeadPath(r Repository) string {
	return filepath.Join(g.repoPath(r), "tinyrelay.pending.head")
}

func (g *GitRelay) stateObjectsPresent(ctx context.Context, r Repository) bool {
	oids := make([]string, 0, len(r.Refs))
	for _, oid := range r.Refs {
		if oid != "" {
			oids = append(oids, oid)
		}
	}
	return g.objectsPresent(ctx, r, oids)
}

// objectsPresent checks all object IDs through one long-lived cat-file
// process. Repository state can contain hundreds of refs; starting one Git
// process per ref makes promotion latency grow linearly with process startup.
func (g *GitRelay) objectsPresent(ctx context.Context, r Repository, oids []string) bool {
	if len(oids) == 0 {
		return true
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir", g.repoPath(r), "cat-file", "--batch-check")
	var out bytes.Buffer
	cmd.Stdin = strings.NewReader(strings.Join(oids, "\n") + "\n")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return false
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != len(oids) {
		return false
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] == "missing" {
			return false
		}
	}
	return true
}
func (g *GitRelay) ensureRepo(r Repository) error {
	p := g.repoPath(r)
	if _, err := os.Stat(filepath.Join(p, "HEAD")); err == nil {
		return g.configureRepo(r)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	cmd := exec.Command("git", "init", "--bare", p)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return g.configureRepo(r)
}

func (g *GitRelay) configureRepo(r Repository) error {
	g.repoConfigMu.Lock()
	defer g.repoConfigMu.Unlock()
	if _, ok := g.configured[g.repoPath(r)]; ok {
		return nil
	}
	for _, pair := range [][2]string{{"http.receivepack", "true"}, {"http.uploadpack", "true"}} {
		cmd := exec.Command("git", "--git-dir", g.repoPath(r), "config", pair[0], pair[1])
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git config %s: %w: %s", pair[0], err, strings.TrimSpace(string(out)))
		}
	}
	if err := g.installHook(r); err != nil {
		return err
	}
	if g.configured == nil {
		g.configured = make(map[string]struct{})
	}
	g.configured[g.repoPath(r)] = struct{}{}
	return nil
}

func (g *GitRelay) installHook(r Repository) error {
	hook := filepath.Join(g.repoPath(r), "hooks", "pre-receive")
	state, pending := g.statePath(r), g.pendingStatePath(r)
	// Load signed maps once per push, not one process per ref. The hook is
	// static; metadata changes only replace the signed ref files.
	script := "#!/bin/sh\nset -eu\nTINY_REFS=" + shellQuote(state) + " TINY_PENDING=" + shellQuote(pending) + ` awk '
BEGIN {
  while ((getline line < ENVIRON["TINY_REFS"]) > 0) { split(line, row, " "); expected[row[1]]=row[2] }
  while ((getline line < ENVIRON["TINY_PENDING"]) > 0) { split(line, row, " "); expected[row[1]]=row[2] }
}
NF != 3 || expected[$3] != $2 {
  print "tinyrelay: ref is not authorized by signed repository state" > "/dev/stderr"
  exit 1
}
'
`
	if err := atomicExecutable(hook, []byte(script)); err != nil {
		return err
	}
	post := filepath.Join(g.repoPath(r), "hooks", "post-receive")
	postScript := "#!/bin/sh\nset -eu\nif [ -f " + shellQuote(pending) + " ]; then\n  checked=$(awk '{print $2}' " + shellQuote(pending) + " | git cat-file --batch-check) || exit 1\n  if printf '%s\\n' \"$checked\" | awk 'NF != 3 || $2 == \"missing\" { bad=1 } END { exit bad }'; then mv -f " + shellQuote(pending) + " " + shellQuote(state) + "; fi\nfi\n"
	return atomicExecutable(post, []byte(postScript))
}

func atomicExecutable(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tinyrelay-hook-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0700); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func (g *GitRelay) writeState(r Repository) error {
	if err := g.ensureRepo(r); err != nil {
		return err
	}
	if err := g.projectNativeRefs(r, r.Refs); err != nil {
		return err
	}
	if err := g.writeRefsFile(g.statePath(r), r.Refs); err != nil {
		return err
	}
	if r.Head != "" {
		ref := strings.TrimPrefix(r.Head, "ref: ")
		if out, err := exec.Command("git", "--git-dir", g.repoPath(r), "symbolic-ref", "HEAD", ref).CombinedOutput(); err != nil {
			return fmt.Errorf("install Git HEAD: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func (g *GitRelay) projectNativeRefs(r Repository, next map[string]string) error {
	if len(next) > 0 && !g.objectsPresent(context.Background(), r, refObjectIDs(next)) {
		// Alternative PR metadata may arrive before its object. Keep the
		// signed file authoritative for the receive hook, but do not create a
		// native ref Git cannot resolve until the object is present.
		return nil
	}
	previous, err := g.nativeRefs(r)
	if err != nil {
		return err
	}
	refs := make(map[string]struct{}, len(previous)+len(next))
	for ref := range previous {
		refs[ref] = struct{}{}
	}
	for ref := range next {
		if !internalRef(ref) {
			refs[ref] = struct{}{}
		}
	}
	keys := make([]string, 0, len(refs))
	for ref := range refs {
		keys = append(keys, ref)
	}
	sort.Strings(keys)
	var tx strings.Builder
	tx.WriteString("start\n")
	for _, ref := range keys {
		if oid, ok := next[ref]; ok && !internalRef(ref) {
			if previousOID, exists := previous[ref]; exists && previousOID == oid {
				continue
			}
			tx.WriteString("update ")
			tx.WriteString(ref)
			tx.WriteByte(' ')
			tx.WriteString(oid)
			tx.WriteByte('\n')
			continue
		}
		if _, ok := previous[ref]; ok {
			tx.WriteString("delete ")
			tx.WriteString(ref)
			tx.WriteByte('\n')
		}
	}
	tx.WriteString("prepare\ncommit\n")
	cmd := exec.Command("git", "--git-dir", g.repoPath(r), "update-ref", "--stdin")
	cmd.Stdin = strings.NewReader(tx.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("project Git refs: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (g *GitRelay) nativeRefs(r Repository) (map[string]string, error) {
	cmd := exec.Command("git", "--git-dir", g.repoPath(r), "for-each-ref", "--format=%(refname) %(objectname)")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read native Git refs: %w", err)
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && !internalRef(fields[0]) {
			refs[fields[0]] = fields[1]
		}
	}
	return refs, nil
}

func refObjectIDs(refs map[string]string) []string {
	oids := make([]string, 0, len(refs))
	for _, oid := range refs {
		if oid != "" {
			oids = append(oids, oid)
		}
	}
	return oids
}

func internalRef(ref string) bool { return strings.HasPrefix(ref, "refs/tinyrelay/") }

func readRefsFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			refs[fields[0]] = fields[1]
		}
	}
	return refs, nil
}

func (g *GitRelay) writePendingState(r Repository) error {
	if err := g.ensureRepo(r); err != nil {
		return err
	}
	if err := g.writeRefsFile(g.pendingStatePath(r), r.Refs); err != nil {
		return err
	}
	if r.Head == "" {
		return nil
	}
	return os.WriteFile(g.pendingHeadPath(r), []byte(r.Head+"\n"), 0600)
}

func (g *GitRelay) writeRefsFile(path string, refs map[string]string) error {
	keys := make([]string, 0, len(refs))
	for ref := range refs {
		keys = append(keys, ref)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, ref := range keys {
		b.WriteString(ref)
		b.WriteByte(' ')
		b.WriteString(refs[ref])
		b.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0600); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (g *GitRelay) lookup(owner, id string) (Repository, error) {
	g.mu.RLock()
	r, ok := g.repos[key(owner, id)]
	g.mu.RUnlock()
	if ok {
		return r, nil
	}
	q, err := g.store.Query(context.Background(), event.Filter{Authors: []string{owner}, Kinds: []int{30617}, Tags: map[string][]string{"d": {id}}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
	if err != nil {
		return Repository{}, err
	}
	if len(q.Events) == 0 {
		return Repository{}, os.ErrNotExist
	}
	r, err = g.parseRepository(q.Events[0])
	if err != nil {
		return Repository{}, err
	}
	// The event store may acknowledge a state before the asynchronous Git
	// work intent has run. Load the latest signed state here so an immediate
	// receive-pack request can stage its authorization hook synchronously.
	// A maintainer, including an agent the host vouches for, may have signed
	// it; parseRepository rejects any author who is not one.
	states, stateErr := g.store.Query(context.Background(), event.Filter{Kinds: []int{30618}, Tags: map[string][]string{"d": {id}}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 8})
	if stateErr == nil {
		for _, candidate := range states.Events {
			state, parseErr := g.parseRepository(candidate)
			if parseErr != nil || state.Owner != owner {
				continue
			}
			state.Clone, state.Relays, state.Maintainers = r.Clone, r.Relays, r.Maintainers
			r = state
			break
		}
	}
	g.mu.Lock()
	g.repos[key(owner, id)] = r
	g.mu.Unlock()
	return r, nil
}

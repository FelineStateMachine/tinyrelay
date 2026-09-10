package gitrelay

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// ValidateEvent checks the event's signature and structural validity.
func (g *GitRelay) ValidateEvent(e event.Event) error { return event.Validate(e) }

// Validate performs GRASP admission and returns the repository authority
// that the caller must associate with its event transaction. It has no store
// side effects.
func (g *GitRelay) Validate(ctx context.Context, e event.Event) (Repository, error) {
	if err := event.Validate(e); err != nil {
		return Repository{}, err
	}
	if e.Kind != 30617 && e.Kind != 30618 {
		return Repository{}, fmt.Errorf("unsupported: event kind %d is not GRASP repository metadata", e.Kind)
	}
	r, err := g.parseRepository(e)
	if err != nil {
		return Repository{}, err
	}
	if e.Kind == 30617 {
		if err := g.validateArchiveAnnouncement(r); err != nil {
			return Repository{}, err
		}
	}
	if g.authorize != nil {
		if err := g.authorize(ctx, e, r); err != nil {
			return Repository{}, err
		}
	} else if !g.IsMaintainer(ctx, r, e.PubKey) {
		return Repository{}, errors.New("blocked: repository metadata must be signed by owner")
	}
	if e.Kind == 30618 {
		if r.EventID == "" {
			return Repository{}, errors.New("blocked: repository announcement is missing")
		}
		if err := g.validateRefs(r.Refs); err != nil {
			return Repository{}, err
		}
		// Missing objects are a durable pending transition. The signed state is
		// accepted, but remains invisible until receive-pack supplies every tip.
	}
	return r, nil
}

// CommitAfterStore applies the Git-side transition after the caller has
// durably saved the event. It intentionally does not call Store.Save.
func (g *GitRelay) CommitAfterStore(ctx context.Context, e event.Event, repo Repository) error {
	return g.commitAfterStore(ctx, e, repo, true)
}

// CommitAfterStoreNoNotify stages the Git transition without invoking the
// host fanout callback. Use this while already inside the relay publish
// fence; the durable work intent will release visibility afterward.
func (g *GitRelay) CommitAfterStoreNoNotify(ctx context.Context, e event.Event, repo Repository) error {
	return g.commitAfterStore(ctx, e, repo, false)
}

func (g *GitRelay) commitAfterStore(ctx context.Context, e event.Event, repo Repository, notify bool) error {
	g.commitMu.Lock()
	err := g.stageAfterStore(ctx, e, repo)
	pending := g.IsPending(e.ID)
	g.commitMu.Unlock()
	// The host callback enters its publish fence. Never hold the Git lock
	// here: synchronous publication already enters these locks in reverse.
	if err == nil && !pending && notify && g.onPromote != nil {
		return g.onPromote(ctx, e.ID, repo)
	}
	return err
}

func (g *GitRelay) stageAfterStore(ctx context.Context, e event.Event, repo Repository) error {
	current, err := g.currentMetadata(ctx, e)
	if err != nil {
		return err
	}
	if !current {
		// A queued intent may outlive a newer addressable announcement/state.
		// Never let that stale intent replace the currently authorized hook.
		return nil
	}
	if e.Kind == 30617 {
		// Announcements and ref state are separate addressable events. An
		// announcement update must retain the current signed refs and HEAD.
		var raw string
		err := g.store.DB().QueryRowContext(ctx, `SELECT raw FROM events WHERE kind=30618 AND pubkey=? AND d=? ORDER BY created_at DESC,id ASC LIMIT 1`, e.PubKey, event.Tag(e, "d")).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			state, err := event.Parse([]byte(raw))
			if err != nil {
				return err
			}
			stateRepo, err := g.parseRepository(state)
			if err != nil {
				return err
			}
			repo.Refs, repo.Head, repo.EventID = stateRepo.Refs, stateRepo.Head, state.ID
		}
	}
	jr := journalRecord{Repository: key(repo.Owner, repo.Identifier), EventID: e.ID, Kind: e.Kind, Refs: repo.Refs, Head: repo.Head}
	if err := g.writeJournal(repo, jr); err != nil {
		return err
	}
	g.mu.Lock()
	g.repos[key(repo.Owner, repo.Identifier)] = repo
	g.mu.Unlock()
	if err := g.ensureRepo(repo); err != nil {
		return err
	}
	pending := false
	if e.Kind == 30618 {
		var err error
		if g.stateObjectsPresent(ctx, repo) {
			err = g.writeState(repo)
		} else {
			err = g.writePendingState(repo)
			g.mu.Lock()
			g.pending[e.ID] = struct{}{}
			g.mu.Unlock()
			pending = true
		}
		if err != nil {
			return err
		}
	}
	if pending {
		return nil
	}
	if err := g.commitJournal(repo, jr); err != nil {
		return err
	}
	return nil
}

func (g *GitRelay) currentMetadata(ctx context.Context, e event.Event) (bool, error) {
	if e.Kind != 30617 && e.Kind != 30618 {
		return true, nil
	}
	var id string
	err := g.store.DB().QueryRowContext(ctx, `SELECT events.id FROM events JOIN tags ON tags.event_id=events.id AND tags.name='d' AND tags.value=? WHERE events.pubkey=? AND events.kind=? ORDER BY events.created_at DESC,events.id ASC LIMIT 1`, event.Tag(e, "d"), e.PubKey, e.Kind).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return id == e.ID, nil
}

// IsPending lets the event admission/read layer hide a metadata event until
// its signed Git tips are verified locally.
func (g *GitRelay) IsPending(eventID string) bool {
	g.mu.RLock()
	_, ok := g.pending[eventID]
	g.mu.RUnlock()
	return ok
}

// ExpirePending removes the file and in-memory authorities for state events
// whose retained deadline has elapsed. The SQL event sweep calls this before
// deleting the corresponding event so a stale journal cannot promote it after
// a restart.
func (g *GitRelay) ExpirePending(ctx context.Context, eventIDs []string) error {
	if g == nil || len(eventIDs) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(eventIDs))
	for _, id := range eventIDs {
		want[id] = struct{}{}
	}
	entries, err := os.ReadDir(g.journalDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(g.journalDir(), entry.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var jr journalRecord
		if err := json.Unmarshal(body, &jr); err != nil {
			return err
		}
		if _, ok := want[jr.EventID]; !ok {
			continue
		}
		owner, identifier, ok := strings.Cut(jr.Repository, "\x00")
		if ok {
			_ = os.Remove(g.pendingStatePath(Repository{Owner: owner, Identifier: identifier}))
			_ = os.Remove(g.pendingHeadPath(Repository{Owner: owner, Identifier: identifier}))
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	g.mu.Lock()
	for id := range want {
		delete(g.pending, id)
	}
	g.mu.Unlock()
	return ctx.Err()
}

// PruneRef removes an expired native Git ref before its retained metadata is
// deleted. repository may be the storage key or its identifier.
func (g *GitRelay) PruneRef(ctx context.Context, repository, ref string) error {
	if g == nil || !validRef(ref) {
		return errors.New("git relay: invalid expired ref")
	}
	g.mu.RLock()
	var found *Repository
	for _, candidate := range g.repos {
		if key(candidate.Owner, candidate.Identifier) == repository || candidate.Identifier == repository {
			copy := candidate
			found = &copy
			break
		}
	}
	g.mu.RUnlock()
	if found == nil {
		return nil
	}
	if err := exec.CommandContext(ctx, "git", "--git-dir", g.repoPath(*found), "update-ref", "-d", ref).Run(); err != nil {
		return err
	}
	g.mu.Lock()
	if current, ok := g.repos[key(found.Owner, found.Identifier)]; ok {
		delete(current.Refs, ref)
		g.repos[key(found.Owner, found.Identifier)] = current
	}
	g.mu.Unlock()
	return nil
}

// PromotePending verifies and exposes a previously accepted state event. It
// is useful for local receive-pack integrations; ServeHTTP invokes it after a
// smart push as well.
func (g *GitRelay) PromotePending(ctx context.Context, repo Repository) error {
	g.commitMu.Lock()
	promoted, err := g.promotePending(ctx, repo)
	g.commitMu.Unlock()
	if err == nil && promoted && g.onPromote != nil {
		return g.onPromote(ctx, repo.EventID, repo)
	}
	return err
}

func (g *GitRelay) promotePending(ctx context.Context, repo Repository) (bool, error) {
	var current int
	if err := g.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE id=? AND kind=30618`, repo.EventID).Scan(&current); err != nil {
		return false, err
	}
	if current == 0 {
		return false, nil
	}
	if !g.stateObjectsPresent(ctx, repo) {
		return false, nil
	}
	g.mu.RLock()
	_, tracked := g.pending[repo.EventID]
	g.mu.RUnlock()
	if _, err := os.Stat(g.pendingStatePath(repo)); err != nil && !tracked {
		return false, nil
	}
	if err := g.writeState(repo); err != nil {
		return false, err
	}
	_ = os.Remove(g.pendingStatePath(repo))
	_ = os.Remove(g.pendingHeadPath(repo))
	g.mu.Lock()
	delete(g.pending, repo.EventID)
	g.mu.Unlock()
	jr := journalRecord{Repository: key(repo.Owner, repo.Identifier), EventID: repo.EventID, Kind: 30618, Refs: repo.Refs, Head: repo.Head}
	if err := g.commitJournal(repo, jr); err != nil {
		return false, err
	}
	return true, nil
}

// Publish ingests a signed repository announcement or state event. Callers
// must validate the event before invoking this method; ValidateEvent is
// provided for callers that want one explicit boundary.
func (g *GitRelay) Publish(ctx context.Context, e event.Event) error {
	if err := event.Validate(e); err != nil {
		return err
	}
	if e.Kind != 30617 && e.Kind != 30618 {
		if !((g.grasp06 || g.policy().Features.Grasp06) && (e.Kind == 1617 || e.Kind == 1618 || e.Kind == 1619)) {
			return fmt.Errorf("unsupported: event kind %d is not GRASP repository metadata", e.Kind)
		}
		g.commitMu.Lock()
		defer g.commitMu.Unlock()
		return g.publishPR(ctx, e)
	}
	repo, err := g.Validate(ctx, e)
	if err != nil {
		return err
	}
	if _, err := g.store.Save(ctx, e, storage.SaveOptions{Now: time.Now().Unix()}); err != nil {
		return err
	}
	return g.CommitAfterStore(ctx, e, repo)
}

func (g *GitRelay) parseRepository(e event.Event) (Repository, error) {
	id := event.Tag(e, "d")
	if !validIdentifier(id) {
		return Repository{}, errors.New("invalid: repository identifier")
	}
	r := Repository{Owner: e.PubKey, Identifier: id, EventID: e.ID, Refs: map[string]string{}}
	if e.Kind == 30617 {
		for _, tag := range e.Tags {
			if len(tag) < 2 {
				continue
			}
			switch tag[0] {
			case "private":
				r.Private = tag[1] == "true"
			case "clone":
				r.Clone = append(r.Clone, tag[1:]...)
			case "relays":
				r.Relays = append(r.Relays, tag[1:]...)
			case "maintainers":
				r.Maintainers = append(r.Maintainers, tag[1:]...)
			}
		}
		return r, nil
	}
	// State is replaceable: bind it to the current announcement owner and
	// carry the announcement's identity into the hook/state file.
	state := Repository{Owner: e.PubKey, Identifier: id, EventID: e.ID, Refs: map[string]string{}}
	for _, tag := range e.Tags {
		if len(tag) == 0 {
			continue
		}
		if tag[0] == "HEAD" {
			if len(tag) != 2 || state.Head != "" {
				return Repository{}, errors.New("invalid: repository state has multiple HEAD tags")
			}
			state.Head = tag[1]
			continue
		}
		if len(tag) < 2 {
			continue
		}
		if strings.HasPrefix(tag[0], "refs/") {
			if _, duplicate := state.Refs[tag[0]]; duplicate || tag[1] == "" || !isObjectID(tag[1]) || strings.Trim(tag[1], "0") == "" {
				return Repository{}, fmt.Errorf("invalid: ref %s is not a SHA-1", tag[0])
			}
			state.Refs[tag[0]] = tag[1]
		}
	}
	g.mu.RLock()
	ann := g.repos[key(e.PubKey, id)]
	g.mu.RUnlock()
	if ann.Owner == "" {
		q, err := g.store.Query(context.Background(), event.Filter{Kinds: []int{30617}, Tags: map[string][]string{"d": {id}}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 0})
		if err != nil {
			return Repository{}, err
		}
		if len(q.Events) == 0 {
			return Repository{}, errors.New("blocked: repository announcement is missing")
		}
		for _, candidate := range q.Events {
			ann, err = g.parseRepository(candidate)
			if err == nil && g.IsMaintainer(context.Background(), ann, e.PubKey) {
				break
			}
			ann = Repository{}
		}
		if ann.Owner == "" {
			return Repository{}, errors.New("blocked: state author is not an accepted repository maintainer")
		}
	}
	if !g.IsMaintainer(context.Background(), ann, e.PubKey) {
		return Repository{}, errors.New("blocked: state author is not an accepted repository maintainer")
	}
	state.Owner, state.Private = ann.Owner, ann.Private
	state.Clone = append([]string(nil), ann.Clone...)
	state.Relays = append([]string(nil), ann.Relays...)
	state.Maintainers = append([]string(nil), ann.Maintainers...)
	state.EventID = e.ID
	if err := validateHead(state.Head, state.Refs); err != nil {
		return Repository{}, err
	}
	return state, nil
}

func validateHead(head string, refs map[string]string) error {
	if head == "" {
		return nil
	}
	if !strings.HasPrefix(head, "ref: ") {
		return errors.New("invalid: HEAD must use ref: syntax")
	}
	ref := strings.TrimPrefix(head, "ref: ")
	if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		return errors.New("invalid: HEAD must name a branch or tag")
	}
	if !validRef(ref) {
		return errors.New("invalid: HEAD ref")
	}
	return nil
}

func (g *GitRelay) validateRefs(refs map[string]string) error {
	for ref, oid := range refs {
		if !validRef(ref) || oid != "" && !isObjectID(oid) {
			return fmt.Errorf("invalid: ref %s", ref)
		}
	}
	return nil
}

// ErrIncomplete reports a synchronization pass that saved progress and has
// more history to fetch. The scheduler continues it soon without counting a
// failure.
var ErrIncomplete = errors.New("synchronization incomplete")

// Capabilities returns only protocol surfaces that this instance can serve.
func (g *GitRelay) Capabilities() []string {
	p := g.policy()
	c := []string{"GRASP-01"}
	if p.Features.Grasp02 && g.eventSync != nil {
		c = append(c, "GRASP-02")
		if p.Features.Grasp03 {
			c = append(c, "GRASP-03")
		}
	}
	if g.grasp06 || p.Features.Grasp06 {
		c = append(c, "GRASP-06")
	}
	if p.PrivateServiceEnabled() {
		c = append(c, "GRASP-08")
	}
	return c
}

// SupportedGRASPs is the NIP-11-facing capability list. It deliberately
// mirrors Capabilities so callers cannot advertise preview protocols by hand.
func (g *GitRelay) SupportedGRASPs() []string { return g.Capabilities() }

// DeletePRRef removes a temporary GRASP-06 ref before its Nostr metadata is
// deleted. Identity accepts the source coordinates used by delivery jobs:
// pr:<owner>:<identifier> or 30617:<owner>:<identifier>.
func (g *GitRelay) DeletePRRef(ctx context.Context, identity, ref string) error {
	parts := strings.SplitN(identity, ":", 3)
	if len(parts) != 3 || (parts[0] != "pr" && parts[0] != "30617") || !isPubKey(parts[1]) || parts[2] == "" || strings.ContainsAny(parts[2], `/\\`) {
		return errors.New("invalid: PR repository identity")
	}
	if !strings.HasPrefix(ref, "refs/nostr/") || len(strings.TrimPrefix(ref, "refs/nostr/")) != 64 {
		return errors.New("invalid: PR ref")
	}
	r := Repository{Owner: parts[1], Identifier: parts[2], Alternative: true, Refs: map[string]string{}}
	if err := g.ensureRepo(r); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir", g.repoPath(r), "update-ref", "-d", ref)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("delete PR ref: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return g.removeRefFromPending(r, ref)
}

func (g *GitRelay) removeRefFromPending(r Repository, ref string) error {
	b, err := os.ReadFile(g.pendingStatePath(r))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var kept map[string]string = map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] != ref {
			kept[fields[0]] = fields[1]
		}
	}
	if len(kept) == 0 {
		return os.Remove(g.pendingStatePath(r))
	}
	return g.writeRefsFile(g.pendingStatePath(r), kept)
}

// Reload rebuilds repository and pending indexes after a restore has inserted
// events and native Git repositories on disk.
func (g *GitRelay) Reload(ctx context.Context) error {
	g.commitMu.Lock()
	defer g.commitMu.Unlock()
	g.repoConfigMu.Lock()
	g.configured = nil
	g.repoConfigMu.Unlock()
	if err := g.recoverJournals(); err != nil {
		return err
	}
	q, err := g.store.Query(ctx, event.Filter{Kinds: []int{30617}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 0})
	if err != nil {
		return err
	}
	repos := make(map[string]Repository)
	for _, raw := range q.Events {
		r, parseErr := g.parseRepository(raw)
		if parseErr == nil {
			repos[key(r.Owner, r.Identifier)] = r
		}
	}
	// Recover only native refs that match the published signed authority.
	// Incoming receive-pack refs may still be awaiting promotion.
	for coordinate, announcement := range repos {
		if err := g.ensureRepo(announcement); err != nil {
			return err
		}
		refs, refsErr := g.publishedNativeRefs(announcement)
		if refsErr != nil {
			return refsErr
		}
		if len(refs) > 0 {
			announcement.Refs = refs
			if head, headErr := exec.Command("git", "--git-dir", g.repoPath(announcement), "symbolic-ref", "-q", "HEAD").Output(); headErr == nil {
				announcement.Head = "ref: " + strings.TrimSpace(string(head))
			}
			repos[coordinate] = announcement
		}
	}
	// If no native refs exist (for example an older store written before native
	// projection), recover the newest complete signed state as a fallback.
	for coordinate, announcement := range repos {
		if _, err := os.Stat(g.statePath(announcement)); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err := os.Stat(g.pendingStatePath(announcement)); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		states, stateErr := g.store.Query(ctx, event.Filter{Authors: append([]string{announcement.Owner}, announcement.Maintainers...), Kinds: []int{30618}, Tags: map[string][]string{"d": {announcement.Identifier}}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 0})
		if stateErr != nil {
			return stateErr
		}
		if len(states.Events) == 0 {
			continue
		}
		sort.Slice(states.Events, func(i, j int) bool { return states.Events[i].CreatedAt > states.Events[j].CreatedAt })
		state, parseErr := g.parseRepository(states.Events[0])
		if parseErr != nil || state.Owner != announcement.Owner || !g.stateObjectsPresent(ctx, state) {
			continue
		}
		repos[coordinate] = state
	}
	prs, prErr := g.store.Query(ctx, event.Filter{Kinds: []int{1617, 1618, 1619}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 0})
	if prErr != nil {
		return prErr
	}
	for _, raw := range prs.Events {
		identifier := event.Tag(raw, "d")
		if identifier == "" {
			identifier = event.Tag(raw, "a")
		}
		commit := event.Tag(raw, "c")
		if !validIdentifier(identifier) || !isObjectID(commit) {
			continue
		}
		pr := Repository{Owner: raw.PubKey, Identifier: identifier, EventID: raw.ID, Alternative: true, Refs: map[string]string{}}
		if _, err := os.Stat(g.repoPath(pr)); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if refs, refsErr := g.publishedNativeRefs(pr); refsErr == nil && len(refs) > 0 {
			pr.Refs = refs
		}
		repos[key(pr.Owner, pr.Identifier)] = pr
	}
	entries, err := os.ReadDir(g.journalDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	refreshedPending := make(map[string]struct{})
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		b, readErr := os.ReadFile(filepath.Join(g.journalDir(), entry.Name()))
		if readErr != nil {
			return readErr
		}
		var jr journalRecord
		if json.Unmarshal(b, &jr) == nil && jr.EventID != "" {
			refreshedPending[jr.EventID] = struct{}{}
		}
	}
	g.mu.Lock()
	g.repos = repos
	g.pending = refreshedPending
	g.mu.Unlock()
	return nil
}

func (g *GitRelay) publishPR(ctx context.Context, e event.Event) error {
	id := event.Tag(e, "d")
	if id == "" {
		id = event.Tag(e, "a")
	}
	if !validIdentifier(id) {
		return errors.New("invalid: PR repository identifier")
	}
	r := Repository{Owner: e.PubKey, Identifier: id, EventID: e.ID, Alternative: true, Refs: map[string]string{}}
	commit := event.Tag(e, "c")
	if commit != "" && !isObjectID(commit) {
		return errors.New("invalid: PR commit is not a SHA-1")
	}
	if commit != "" {
		r.Refs["refs/nostr/"+e.ID] = commit
	}
	if g.authorize != nil {
		if err := g.authorize(ctx, e, r); err != nil {
			return err
		}
	}
	if err := g.writeJournal(r, journalRecord{Repository: key(r.Owner, r.Identifier), EventID: e.ID, Kind: e.Kind, Refs: r.Refs, Head: r.Head}); err != nil {
		return err
	}
	if _, err := g.store.Save(ctx, e, storage.SaveOptions{Now: time.Now().Unix()}); err != nil {
		return err
	}
	if err := g.ensureRepo(r); err != nil {
		return err
	}
	g.mu.Lock()
	g.repos[key(r.Owner, r.Identifier)] = r
	g.mu.Unlock()
	pending := !g.stateObjectsPresent(ctx, r)
	if !pending {
		if err := g.writeState(r); err != nil {
			return err
		}
	} else {
		if err := g.writePendingState(r); err != nil {
			return err
		}
		g.mu.Lock()
		g.pending[e.ID] = struct{}{}
		g.mu.Unlock()
	}
	if pending {
		return nil
	}
	return g.commitJournal(r, journalRecord{Repository: key(r.Owner, r.Identifier), EventID: e.ID, Kind: e.Kind, Refs: r.Refs, Head: r.Head})
}

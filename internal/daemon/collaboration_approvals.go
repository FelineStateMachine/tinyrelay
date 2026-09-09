package daemon

// Repository proposals. An issue, pull request, patch or comment whose
// author holds an agent grant with propose on that repository is a
// proposal: it stays pending until the repository owner, one of its
// maintainers, the relay owner or a moderator reacts to the event id with
// "+", which approves it, or "-", which rejects it. The newest deciding
// reaction counts; reactions from members and from the author do not. An
// updated event is a new id and starts pending again. A pending or
// rejected proposal is visible only to those deciders and its author; for
// everyone else it does not exist. The state is computed here and carried
// on every browse result that lists repository conversations.

import (
	"context"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// collaborationProposal is the proposal state carried on a listed or shown
// item. Proposal marks an event from an agent whose grant says propose on
// the repository. Approval is pending, approved or rejected; the other
// fields name the deciding reaction, its time and its author once one
// exists.
type collaborationProposal struct {
	Proposal      bool   `json:"proposal,omitempty"`
	Approval      string `json:"approval,omitempty"`
	ApprovalEvent string `json:"approval_event,omitempty"`
	ApprovalAt    int64  `json:"approval_at,omitempty"`
	ApprovalBy    string `json:"approval_by,omitempty"`
}

// collaborationRoles caches the role and grant lookups one browse call
// makes for a repository, since a thread's events and their reactions
// repeat the same few keys.
type collaborationRoles struct {
	t         *Tenant
	repo      gitrelay.Repository
	roles     map[string]string
	grants    map[string]*community.AgentGrant
	proposers map[string]bool
	deciders  map[string]bool
}

func (t *Tenant) collaborationRoles(r gitrelay.Repository) *collaborationRoles {
	return &collaborationRoles{t: t, repo: r, roles: map[string]string{}, grants: map[string]*community.AgentGrant{}, proposers: map[string]bool{}, deciders: map[string]bool{}}
}

func (r *collaborationRoles) role(ctx context.Context, pubkey string) (string, error) {
	if role, ok := r.roles[pubkey]; ok {
		return role, nil
	}
	role, err := r.t.community.Role(ctx, pubkey)
	if err != nil {
		return "", err
	}
	r.roles[pubkey] = role
	return role, nil
}

// grant returns the agent grant recorded for a key, or nil when there is
// none.
func (r *collaborationRoles) grant(ctx context.Context, pubkey string) (*community.AgentGrant, error) {
	if grant, ok := r.grants[pubkey]; ok {
		return grant, nil
	}
	grant, ok, err := r.t.community.AgentGrant(ctx, pubkey)
	if err != nil {
		return nil, err
	}
	var recorded *community.AgentGrant
	if ok {
		recorded = &grant
	}
	r.grants[pubkey] = recorded
	return recorded, nil
}

// decides reports whether a key may approve or reject proposals in the
// repository: its owner, a key in the announcement's maintainers tag, an
// agent whose active grant holds maintain on it, the relay owner and
// moderators.
func (r *collaborationRoles) decides(ctx context.Context, pubkey string) (bool, error) {
	if pubkey == "" {
		return false, nil
	}
	if decides, ok := r.deciders[pubkey]; ok {
		return decides, nil
	}
	decides := pubkey == r.repo.Owner || contains(r.repo.Maintainers, pubkey)
	if !decides {
		role, err := r.role(ctx, pubkey)
		if err != nil {
			return false, err
		}
		decides = wikiDecidingRole(role)
	}
	if !decides {
		grant, err := r.grant(ctx, pubkey)
		if err != nil {
			return false, err
		}
		decides = grant != nil && grant.Maintains(r.repo.Owner, r.repo.Identifier, time.Now().Unix())
	}
	r.deciders[pubkey] = decides
	return decides, nil
}

// proposer reports whether repository events from a key are proposals: the
// key holds an agent grant with propose on the repository and no human
// standing on the relay. A paused, revoked or expired grant keeps counting,
// so an agent's pending events do not surface when its grant ends.
func (r *collaborationRoles) proposer(ctx context.Context, pubkey string) (bool, error) {
	if known, ok := r.proposers[pubkey]; ok {
		return known, nil
	}
	grant, err := r.grant(ctx, pubkey)
	if err != nil {
		return false, err
	}
	proposer := grant != nil && grant.Proposes(r.repo.Owner, r.repo.Identifier)
	if proposer {
		role, err := r.role(ctx, pubkey)
		if err != nil {
			return false, err
		}
		proposer = role == "" || role == "agent"
	}
	r.proposers[pubkey] = proposer
	return proposer, nil
}

// collaborationProposalKind reports whether a kind can be a proposal: the
// kinds a propose grant publishes.
func collaborationProposalKind(kind int) bool {
	return kind == event.KIND_GIT_ISSUE || kind == event.KIND_GIT_PR || kind == event.KIND_GIT_PATCH || kind == 1111
}

// collaborationProposals marks the proposals among rows and resolves each
// one's state from the deciders' reactions to its event id. The result
// runs parallel to rows; the newest deciding reaction counts and the
// author's own never does.
func (t *Tenant) collaborationProposals(ctx context.Context, roles *collaborationRoles, rows []event.Event) ([]collaborationProposal, error) {
	states := make([]collaborationProposal, len(rows))
	var ids []string
	for i, row := range rows {
		if !collaborationProposalKind(row.Kind) {
			continue
		}
		proposer, err := roles.proposer(ctx, row.PubKey)
		if err != nil {
			return nil, err
		}
		if !proposer {
			continue
		}
		states[i] = collaborationProposal{Proposal: true, Approval: wikiProposalPending}
		ids = append(ids, row.ID)
	}
	if len(ids) == 0 {
		return states, nil
	}
	page, err := t.store.Query(ctx, event.Filter{Kinds: []int{kindReaction}, Tags: map[string][]string{"e": ids}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: wikiQueryLimit})
	if err != nil {
		return nil, err
	}
	reactions := page.Events
	sortEventsNewestFirst(reactions)
	for _, reaction := range reactions {
		state := wikiProposalState(reaction.Content)
		if state == "" {
			continue
		}
		decides, err := roles.decides(ctx, reaction.PubKey)
		if err != nil {
			return nil, err
		}
		if !decides {
			continue
		}
		for _, id := range event.TagValues(reaction, "e") {
			for i := range rows {
				s := &states[i]
				if rows[i].ID != id || !s.Proposal || s.ApprovalEvent != "" || rows[i].PubKey == reaction.PubKey {
					continue
				}
				s.Approval, s.ApprovalEvent, s.ApprovalAt, s.ApprovalBy = state, reaction.ID, reaction.CreatedAt, reaction.PubKey
			}
		}
	}
	return states, nil
}

// collaborationVisible resolves the proposal state of rows and keeps the
// ones the actor may see: every row that is not a proposal or is approved,
// plus pending and rejected proposals for the deciders and the proposal's
// author. The returned states run parallel to the kept rows.
func (t *Tenant) collaborationVisible(ctx context.Context, roles *collaborationRoles, actor string, rows []event.Event) ([]event.Event, []collaborationProposal, error) {
	states, err := t.collaborationProposals(ctx, roles, rows)
	if err != nil {
		return nil, nil, err
	}
	decides, err := roles.decides(ctx, actor)
	if err != nil {
		return nil, nil, err
	}
	kept := make([]event.Event, 0, len(rows))
	keptStates := make([]collaborationProposal, 0, len(rows))
	for i, row := range rows {
		s := states[i]
		if !s.Proposal || s.Approval == wikiProposalApproved || decides || (actor != "" && row.PubKey == actor) {
			kept = append(kept, row)
			keptStates = append(keptStates, s)
		}
	}
	return kept, keptStates, nil
}

// collaborationDecided drops status events from proposers, so the status
// of an issue or pull request never takes a pending event into account.
func (t *Tenant) collaborationDecided(ctx context.Context, roles *collaborationRoles, rows []event.Event) ([]event.Event, error) {
	kept := make([]event.Event, 0, len(rows))
	for _, row := range rows {
		proposer, err := roles.proposer(ctx, row.PubKey)
		if err != nil {
			return nil, err
		}
		if !proposer {
			kept = append(kept, row)
		}
	}
	return kept, nil
}

// collaborationCoordinate finds the repository an event points at through
// its first a or A tag.
func collaborationCoordinate(e event.Event) (owner, identifier string, ok bool) {
	for _, tag := range e.Tags {
		if len(tag) < 2 || (tag[0] != "a" && tag[0] != "A") {
			continue
		}
		if owner, identifier, ok = repoCoordinate(tag[1]); ok {
			return owner, identifier, true
		}
	}
	return "", "", false
}

// collaborationProposalNotices wakes the repository owner's and
// maintainers' devices in the approvals category when a proposal arrives.
// It reports false for every other event. A decision wakes nobody.
func (t *Tenant) collaborationProposalNotices(ctx context.Context, e event.Event) ([]pushNotice, bool) {
	if !collaborationProposalKind(e.Kind) || t.git == nil {
		return nil, false
	}
	owner, identifier, ok := collaborationCoordinate(e)
	if !ok {
		return nil, false
	}
	r, err := t.git.BrowseRepository(owner, identifier)
	if err != nil {
		return nil, false
	}
	roles := t.collaborationRoles(r)
	proposer, err := roles.proposer(ctx, e.PubKey)
	if err != nil || !proposer {
		return nil, false
	}
	name := shortKey(e.PubKey)
	if grant, err := roles.grant(ctx, e.PubKey); err == nil && grant != nil && strings.TrimSpace(grant.Name) != "" {
		name = strings.TrimSpace(grant.Name)
	}
	what, subject := "comment", e.Content
	switch e.Kind {
	case event.KIND_GIT_ISSUE:
		what, subject = "issue", collaborationTitle(e)
	case event.KIND_GIT_PR:
		what, subject = "pull request", collaborationTitle(e)
	case event.KIND_GIT_PATCH:
		what, subject = "patch", collaborationTitle(e)
	}
	notice := pushNotice{category: pushApprovals, body: name + " proposes " + what + ": " + excerpt(subject), url: t.collaborationProposalURL(e, r)}
	var notices []pushNotice
	for _, recipient := range uniqueStrings(t.maintainerKeys(ctx, r)) {
		if recipient == e.PubKey {
			continue
		}
		notice.recipient = recipient
		notices = append(notices, notice)
	}
	t.app.telemetry.Logger().Debug("repository proposal notifications built", "count", len(notices))
	return notices, true
}

// collaborationProposalURL is the page a proposal is read on: the issue or
// pull request page for a root and for a comment under one, and the event
// page otherwise.
func (t *Tenant) collaborationProposalURL(e event.Event, r gitrelay.Repository) string {
	base := strings.TrimRight(t.publicURL, "/")
	page := func(view, id string) string {
		return base + "/repo?owner=" + r.Owner + "&repo=" + r.Identifier + "&view=" + view + "&id=" + id
	}
	switch e.Kind {
	case event.KIND_GIT_ISSUE:
		return page("issue", e.ID)
	case event.KIND_GIT_PR:
		return page("pr", e.ID)
	case 1111:
		if root := event.Tag(e, "E"); len(root) == 64 {
			switch event.Tag(e, "K") {
			case "1621":
				return page("issue", root)
			case "1618":
				return page("pr", root)
			}
		}
	}
	return base + "/e/" + e.ID
}

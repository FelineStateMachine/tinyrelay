package daemon

// Wiki proposals. A version (kind 30818) whose author holds an agent grant
// with wiki set to propose is a proposal: it stays pending until the relay
// owner or a moderator reacts to the version's event id with "+", which
// approves it, or "-", which rejects it. A newer version from the agent is
// a new event id and starts pending again, so every edit is decided on its
// own. A pending or rejected proposal is visible only to the owner,
// moderators and its author; everyone else sees the agent's newest
// approved revision in its place, or nothing when none is approved. The
// state is computed here and carried on every browse result that lists
// versions, including the archived revisions in a page's history.

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

const (
	wikiProposalPending  = "pending"
	wikiProposalApproved = "approved"
	wikiProposalRejected = "rejected"
	wikiGrantPropose     = "propose"
)

// wikiRoles caches the role and grant lookups one browse call makes, since
// a page's versions and their reactions repeat the same few keys.
type wikiRoles struct {
	t         *Tenant
	roles     map[string]string
	proposers map[string]bool
}

func (t *Tenant) wikiRoles() *wikiRoles {
	return &wikiRoles{t: t, roles: map[string]string{}, proposers: map[string]bool{}}
}

func (r *wikiRoles) role(ctx context.Context, pubkey string) (string, error) {
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

// decides reports whether a key may approve or reject proposals: the relay
// owner and moderators.
func (r *wikiRoles) decides(ctx context.Context, pubkey string) (bool, error) {
	if pubkey == "" {
		return false, nil
	}
	role, err := r.role(ctx, pubkey)
	return wikiDecidingRole(role), err
}

func wikiDecidingRole(role string) bool {
	return role == "owner" || role == "moderator"
}

// proposer reports whether versions from a key are proposals: the key holds
// an agent grant with wiki set to propose and no human standing on the
// relay. A paused, revoked or expired grant keeps counting, so an agent's
// pending versions do not surface when its grant ends.
func (r *wikiRoles) proposer(ctx context.Context, pubkey string) (bool, error) {
	if known, ok := r.proposers[pubkey]; ok {
		return known, nil
	}
	grant, ok, err := r.t.community.AgentGrant(ctx, pubkey)
	if err != nil {
		return false, err
	}
	proposer := ok && grant.Scope.Wiki == wikiGrantPropose
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

// wikiProposals marks the proposals among versions and resolves each one's
// state from the owner's and moderators' reactions to its event id. The
// newest deciding reaction counts.
func (t *Tenant) wikiProposals(ctx context.Context, roles *wikiRoles, versions []wikiVersion) error {
	var ids []string
	for i := range versions {
		proposer, err := roles.proposer(ctx, versions[i].Author)
		if err != nil {
			return err
		}
		if !proposer {
			continue
		}
		versions[i].Proposal = true
		versions[i].Approval = wikiProposalPending
		ids = append(ids, versions[i].ID)
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := t.store.Query(ctx, event.Filter{Kinds: []int{kindReaction}, Tags: map[string][]string{"e": ids}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: wikiQueryLimit})
	if err != nil {
		return err
	}
	reactions := rows.Events
	sortEventsNewestFirst(reactions)
	for _, reaction := range reactions {
		state := wikiProposalState(reaction.Content)
		if state == "" {
			continue
		}
		decides, err := roles.decides(ctx, reaction.PubKey)
		if err != nil {
			return err
		}
		if !decides {
			continue
		}
		for _, id := range event.TagValues(reaction, "e") {
			for i := range versions {
				v := &versions[i]
				if v.ID != id || !v.Proposal || v.ApprovalEvent != "" {
					continue
				}
				v.Approval, v.ApprovalEvent, v.ApprovalAt, v.ApprovalBy = state, reaction.ID, reaction.CreatedAt, reaction.PubKey
			}
		}
	}
	return nil
}

func wikiProposalState(content string) string {
	switch wikiReactionStatus(content) {
	case wikiMergeAccepted:
		return wikiProposalApproved
	case wikiMergeRejected:
		return wikiProposalRejected
	}
	return ""
}

// wikiSees reports whether an actor may see a version: every version that
// is not a proposal or is approved, plus pending and rejected proposals
// for the owner, moderators (decides) and the proposal's author.
func wikiSees(decides bool, actor string, v wikiVersion) bool {
	return !v.Proposal || v.Approval == wikiProposalApproved || decides || (actor != "" && v.Author == actor)
}

// wikiFallback picks the revision a reader sees in place of a hidden
// version: the same author's newest approved revision of the page among
// the archived revisions, which are sorted newest first.
func wikiFallback(hidden wikiVersion, archived []wikiVersion) (wikiVersion, bool) {
	for _, a := range archived {
		if a.Author == hidden.Author && a.D == hidden.D && a.Proposal && a.Approval == wikiProposalApproved {
			return a, true
		}
	}
	return wikiVersion{}, false
}

// notifyWikiProposal wakes the owner's and moderators' devices in the
// approvals category when a proposal arrives. A decision wakes nobody.
func (t *Tenant) notifyWikiProposal(ctx context.Context, e event.Event) {
	if e.Kind != kindWikiArticle || !t.pushDevicesExist(ctx) {
		return
	}
	roles := t.wikiRoles()
	proposer, err := roles.proposer(ctx, e.PubKey)
	if err != nil || !proposer {
		return
	}
	v := wikiVersionFrom(e, false)
	if v.D == "" {
		return
	}
	name := shortKey(e.PubKey)
	if grant, ok, err := t.community.AgentGrant(ctx, e.PubKey); err == nil && ok && strings.TrimSpace(grant.Name) != "" {
		name = strings.TrimSpace(grant.Name)
	}
	moderators, err := t.community.Moderators(ctx)
	if err != nil {
		return
	}
	notice := pushNotice{category: pushApprovals, body: name + " proposes wiki: " + excerpt(v.Title), url: strings.TrimRight(t.publicURL, "/") + "/wiki/" + url.PathEscape(v.D)}
	queued := 0
	for _, recipient := range uniqueStrings(append([]string{t.Policy().Owner}, moderators...)) {
		if recipient == e.PubKey || !t.pushCoalesced(recipient, pushApprovals) {
			continue
		}
		notice.recipient = recipient
		if err := t.enqueuePushNotice(ctx, notice); err != nil {
			t.app.telemetry.Logger().Debug("wiki proposal notification not queued", "error", err)
			continue
		}
		queued++
	}
	t.app.telemetry.Logger().Debug("wiki proposal notifications queued", "count", queued)
}

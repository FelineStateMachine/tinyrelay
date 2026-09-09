package daemon

import (
	"context"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
)

// Repository maintainers. The owner and the keys in an announcement's
// maintainers tag are maintainers by the event itself; an agent becomes one
// through a kind 30392 grant that holds maintain on that exact repository.
// This file is the one place that answers "does this key maintain this
// repository", for state events and pushes through gitrelay, for status
// changes and access checks in the daemon, and for the browse output that
// the repository panel lists.

// IsMaintainer implements gitrelay.MaintainerSource. A paused, revoked or
// expired grant never counts. Outcomes are recorded under the authorize
// operation; the key itself is never logged or labelled.
func (t *Tenant) IsMaintainer(ctx context.Context, r gitrelay.Repository, pubkey string) bool {
	if pubkey == "" {
		return false
	}
	if pubkey == r.Owner || contains(r.Maintainers, pubkey) {
		return true
	}
	ctx, finish := t.app.telemetry.Start(ctx, "authorize")
	outcome := "unauthorized"
	defer func() { finish(outcome) }()
	grant, ok, err := t.community.AgentGrant(ctx, pubkey)
	if err != nil {
		outcome = "error"
		t.app.telemetry.Logger().WarnContext(ctx, "agent maintainer lookup failed", "error", err)
		return false
	}
	if ok && grant.Maintains(r.Owner, r.Identifier, time.Now().Unix()) {
		outcome = "ok"
		return true
	}
	return false
}

// repositoryMaintainers lists the owner, the announcement's maintainers and
// every agent whose active grant holds maintain on the repository, in that
// order. A key in the maintainers tag that also holds such a grant is
// reported as an agent so the interface can say so.
func (t *Tenant) repositoryMaintainers(ctx context.Context, r gitrelay.Repository) []gitrelay.Maintainer {
	out := []gitrelay.Maintainer{{PubKey: r.Owner, Role: "owner"}}
	seen := map[string]int{r.Owner: 0}
	for _, pubkey := range r.Maintainers {
		if _, dup := seen[pubkey]; dup {
			continue
		}
		seen[pubkey] = len(out)
		out = append(out, gitrelay.Maintainer{PubKey: pubkey, Role: "maintainer"})
	}
	grants, err := t.community.RepositoryAgents(ctx, r.Owner, r.Identifier, time.Now().Unix())
	if err != nil {
		t.app.telemetry.Logger().WarnContext(ctx, "repository agents lookup failed", "error", err)
		return out
	}
	for _, grant := range grants {
		if index, dup := seen[grant.Agent]; dup {
			if index > 0 {
				out[index].Role, out[index].Name = "agent", grant.Name
			}
			continue
		}
		seen[grant.Agent] = len(out)
		out = append(out, gitrelay.Maintainer{PubKey: grant.Agent, Role: "agent", Name: grant.Name})
	}
	return out
}

// maintainerKeys returns the keys whose status events count for a
// repository, for use as an authors filter.
func (t *Tenant) maintainerKeys(ctx context.Context, r gitrelay.Repository) []string {
	maintainers := t.repositoryMaintainers(ctx, r)
	keys := make([]string, 0, len(maintainers))
	for _, maintainer := range maintainers {
		keys = append(keys, maintainer.PubKey)
	}
	return keys
}

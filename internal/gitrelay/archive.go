package gitrelay

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// validateArchiveAnnouncement implements the GRASP-05 admission distinction:
// an archive may announce a repository without clone or relay URLs, but the
// archive profile depends on GRASP-02. A configured service keeps the normal
// GRASP-01 requirement for announcements unless the operator enables both
// extensions. Embedded GitRelay users without a public service URL retain the
// permissive library behavior used by local integrations.
func (g *GitRelay) validateArchiveAnnouncement(r Repository) error {
	if g.serviceURL == "" {
		return nil
	}
	if g.serviceListed(r) {
		return nil
	}
	if g.archiveRelated(context.Background(), r) {
		return nil
	}
	p := policy.Policy{}
	if g.policy != nil {
		p = g.policy()
	}
	if len(r.Clone) == 0 && len(r.Relays) == 0 && p.Features.Grasp02 && p.Features.Grasp05 {
		return nil
	}
	return errors.New("blocked: archive repository admission requires GRASP-02 and GRASP-05")
}

func (g *GitRelay) serviceListed(r Repository) bool {
	base, err := url.Parse(g.serviceURL)
	if err != nil || base.Host == "" {
		return false
	}
	cloneOK, relayOK := false, false
	for _, raw := range r.Clone {
		u, parseErr := url.Parse(raw)
		if parseErr == nil && (u.Scheme == "http" || u.Scheme == "https") && strings.EqualFold(u.Host, base.Host) && strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/"+r.Identifier+".git") {
			cloneOK = true
		}
	}
	for _, raw := range r.Relays {
		u, parseErr := url.Parse(raw)
		if parseErr == nil && (u.Scheme == "ws" || u.Scheme == "wss") && strings.EqualFold(u.Host, base.Host) {
			relayOK = true
		}
	}
	return cloneOK && relayOK
}

func (g *GitRelay) archiveRelated(ctx context.Context, r Repository) bool {
	if g.store == nil {
		return false
	}
	q, err := g.store.Query(ctx, event.Filter{Kinds: []int{30617}, Tags: map[string][]string{"d": {r.Identifier}}}, storage.QueryOptions{Now: 0, Access: storage.Access{All: true}, Limit: 0})
	if err != nil {
		return false
	}
	for _, candidate := range q.Events {
		parsed, parseErr := g.parseRepository(candidate)
		if parseErr != nil || parsed.Identifier != r.Identifier {
			continue
		}
		if candidate.PubKey == r.Owner || contains(parsed.Maintainers, r.Owner) {
			return true
		}
	}
	return false
}

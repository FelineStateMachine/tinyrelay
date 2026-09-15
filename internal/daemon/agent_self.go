package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
)

// browsegrant lets any signed caller see its own standing on the relay: its
// membership and role, the agent grant recorded for its key and what that
// grant covers. Unlike browseagent it needs no operator role, because a key
// only ever learns about itself here. The optional needs let a connector ask
// "may I post these kinds into these rooms" and get back what is missing.

const grantNeedsMax = 256

// grantNeeds is the optional first parameter of browsegrant.
type grantNeeds struct {
	Rooms []string `json:"rooms"`
	Kinds []int    `json:"kinds"`
}

// grantAllows is the scope of the caller's grant as the gate applies it.
type grantAllows struct {
	Kinds []int                 `json:"kinds"`
	Rooms []string              `json:"rooms"`
	Repos []community.AgentRepo `json:"repos"`
	Wiki  string                `json:"wiki"`
	Jobs  string                `json:"jobs"`
	Sites []community.AgentSite `json:"sites"`
}

// grantMissing lists the requested rooms and kinds the caller may not use.
type grantMissing struct {
	Rooms []string `json:"rooms"`
	Kinds []int    `json:"kinds"`
}

// grantSelf is the browsegrant result.
type grantSelf struct {
	Member   bool                    `json:"member"`
	Role     string                  `json:"role"`
	Grant    *community.AgentSummary `json:"grant"`
	State    string                  `json:"state"`
	Enforced bool                    `json:"enforced"`
	Allows   *grantAllows            `json:"allows,omitempty"`
	Rate     int                     `json:"rate,omitempty"`
	Expires  int64                   `json:"expires,omitempty"`
	Missing  *grantMissing           `json:"missing,omitempty"`
}

// grantState names the effective state of one grant.
func grantState(grant community.AgentGrant, now int64) string {
	switch {
	case grant.RevokedAt > 0:
		return "revoked"
	case grant.Paused:
		return "paused"
	case grant.ExpiresAt <= now:
		return "expired"
	}
	return "active"
}

func parseGrantNeeds(params []json.RawMessage) (grantNeeds, bool, error) {
	var needs grantNeeds
	if len(params) == 0 || len(params[0]) == 0 || string(params[0]) == "null" {
		return needs, false, nil
	}
	if err := json.Unmarshal(params[0], &needs); err != nil {
		return needs, false, fmt.Errorf("invalid: browsegrant parameters: %w", err)
	}
	if len(needs.Rooms) > grantNeedsMax || len(needs.Kinds) > grantNeedsMax {
		return needs, false, fmt.Errorf("invalid: browsegrant lists more than %d entries", grantNeedsMax)
	}
	for i, room := range needs.Rooms {
		room = strings.TrimSpace(room)
		if room == "" {
			return needs, false, errors.New("invalid: browsegrant rooms must be room ids")
		}
		needs.Rooms[i] = room
	}
	for _, kind := range needs.Kinds {
		if kind < 0 || kind > 65535 {
			return needs, false, errors.New("invalid: browsegrant kinds must be event kinds")
		}
	}
	return needs, len(needs.Rooms) > 0 || len(needs.Kinds) > 0, nil
}

// browseGrant returns the caller's own grant and effective state. A key
// without a grant gets state "none". A key with a human role is a member in
// its own right; the gate does not apply a grant to its events, so nothing
// is reported missing for it.
func (t *Tenant) browseGrant(ctx context.Context, actor string, params []json.RawMessage) (any, error) {
	needs, asked, err := parseGrantNeeds(params)
	if err != nil {
		return nil, err
	}
	result := grantSelf{State: "none"}
	if actor == "" {
		if asked {
			result.Missing = &grantMissing{Rooms: nonNilStrings(needs.Rooms), Kinds: nonNilInts(needs.Kinds)}
		}
		return result, nil
	}
	role, err := t.community.Role(ctx, actor)
	if err != nil {
		return nil, err
	}
	result.Role = role
	result.Member = role != ""
	result.Enforced = role == "" || role == "agent"
	grant, ok, err := t.community.AgentGrant(ctx, actor)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	if ok {
		summary := community.AgentSummary{AgentGrant: grant}
		_ = t.store.DB().QueryRowContext(ctx, `SELECT coalesce(max(created_at),0) FROM events WHERE pubkey=?`, actor).Scan(&summary.LastEvent)
		result.Grant = &summary
		result.State = grantState(grant, now)
		result.Rate = grant.Scope.Rate
		result.Expires = grant.ExpiresAt
		result.Allows = &grantAllows{Kinds: nonNilInts(grant.Scope.Kinds), Rooms: nonNilStrings(grant.Scope.Rooms), Repos: grant.Scope.Repos, Wiki: grant.Scope.Wiki, Jobs: grant.Scope.Jobs, Sites: grant.Scope.Sites}
		if result.Allows.Repos == nil {
			result.Allows.Repos = []community.AgentRepo{}
		}
		if result.Allows.Sites == nil {
			result.Allows.Sites = []community.AgentSite{}
		}
	}
	if !asked {
		return result, nil
	}
	missing := &grantMissing{Rooms: []string{}, Kinds: []int{}}
	if result.Enforced {
		for _, room := range needs.Rooms {
			if !ok || !grant.AllowsRoom(room) {
				missing.Rooms = append(missing.Rooms, room)
			}
		}
		for _, kind := range needs.Kinds {
			if !ok || !grant.AllowsKind(kind) {
				missing.Kinds = append(missing.Kinds, kind)
			}
		}
	}
	result.Missing = missing
	return result, nil
}

func nonNilInts(values []int) []int {
	if values == nil {
		return []int{}
	}
	return values
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

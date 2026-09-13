package tinygit

import (
	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
)

// Event is the signed Nostr wire event accepted by the Git engine. Clients
// sign events themselves; the server never needs their private keys.
type Event = nostr.Event

// Policy contains the policy values used by the Git engine. Hosts may keep a
// richer policy internally and adapt these values at the integration boundary.
type Policy struct {
	Owner        string
	Reads        string
	PrivatePeers []string
	Features     PolicyFeatures
}

// PolicyFeatures selects optional Git protocol capabilities.
type PolicyFeatures struct {
	Grasp   bool
	Grasp02 bool
	Grasp03 bool
	Grasp05 bool
	Grasp06 bool
	Grasp08 bool
}

func (p Policy) PrivateServiceEnabled() bool {
	return p.Features.Grasp && p.Features.Grasp08 && p.Reads == "members"
}

// DefaultPolicy returns the defaults for an embedded engine.
func DefaultPolicy(owner string) Policy {
	return Policy{Owner: owner, Reads: "open"}
}

func (g *GitRelay) policyValue() Policy {
	if g.policy == nil {
		return DefaultPolicy("")
	}
	return g.policy()
}

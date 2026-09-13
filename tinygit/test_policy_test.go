package tinygit

import (
	"github.com/FelineStateMachine/tinyrelay/internal/gitstore"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func tinyStore(store *storage.Store) Store { return gitstore.Wrap(store) }

func fromInternalPolicy(p policy.Policy) Policy {
	return Policy{Owner: p.Owner, Reads: p.Reads, PrivatePeers: append([]string(nil), p.PrivatePeers...), Features: PolicyFeatures{Grasp: p.Features.Grasp, Grasp02: p.Features.Grasp02, Grasp03: p.Features.Grasp03, Grasp05: p.Features.Grasp05, Grasp06: p.Features.Grasp06, Grasp08: p.Features.Grasp08}}
}

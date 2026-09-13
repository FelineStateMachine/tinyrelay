package auth

import (
	"time"

	proof "github.com/FelineStateMachine/tinyrelay/protocol/auth"
)

// Validator and ChallengeManager preserve public verifier and replay-store
// identity for callers that have not migrated to protocol/auth.
type Validator = proof.Validator
type ChallengeManager = proof.ChallengeManager

var ErrMissingAuthorization = proof.ErrMissingAuthorization
var ErrGRASP08Unauthorized = proof.ErrGRASP08Unauthorized

func NewValidator(now func() time.Time) *Validator { return proof.NewValidator(now) }
func NewChallengeManager(relayURL string, now func() time.Time) *ChallengeManager {
	return proof.NewChallengeManager(relayURL, now)
}
func IsBlossomAuthorization(header string) bool       { return proof.IsBlossomAuthorization(header) }
func GRASP08RepositoryRoot(raw string) (string, bool) { return proof.GRASP08RepositoryRoot(raw) }

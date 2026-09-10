package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

type privateGitProofKey struct{}
type privateGitPayloadCheckedKey struct{}

func privateGitRequest(r *http.Request, proof event.Event) *http.Request {
	ctx := context.WithValue(r.Context(), privateGitProofKey{}, proof)
	ctx = context.WithValue(ctx, privateGitPayloadCheckedKey{}, struct{}{})
	return r.WithContext(ctx)
}

func privateGitProof(r *http.Request) (event.Event, bool) {
	proof, ok := r.Context().Value(privateGitProofKey{}).(event.Event)
	return proof, ok
}

// PrivateServiceProfile describes a GRASP-08 tenant's configured protocol,
// authentication methods, read policy and owner identity. Protocol adapters
// choose the fields appropriate to the request's authenticated audience.
type PrivateServiceProfile struct {
	Enabled        bool
	Protocol       string
	Authentication []string
	Reads          string
	Owner          string
}

// PrivateAccess records tenant-level authority resolved from current community
// membership. Owner, member and moderator roles grant private-service access.
type PrivateAccess struct {
	PubKey string
	Role   string
	Member bool
	Owner  bool
}

func (a PrivateAccess) Allowed() bool { return a.Member || a.Owner }

// PrivateServiceEnabled reports whether this tenant has the complete private
// service contract enabled. Callers should use PrivateAccess before exposing
// any tenant or repository metadata.
func (t *Tenant) PrivateServiceEnabled() bool {
	return t != nil && t.Policy().PrivateServiceEnabled()
}

func (t *Tenant) PrivateProfile() PrivateServiceProfile {
	if t == nil {
		return PrivateServiceProfile{}
	}
	p := t.Policy()
	return PrivateServiceProfile{
		Enabled:        p.PrivateServiceEnabled(),
		Protocol:       "GRASP-08",
		Authentication: []string{"NIP-42", "NIP-98"},
		Reads:          p.Reads,
		Owner:          p.Owner,
	}
}

// PrivateAccess reads current membership and bans for each request, so access
// changes apply to the next authorization check.
func (t *Tenant) PrivateAccess(ctx context.Context, pubkey string) (PrivateAccess, error) {
	access := PrivateAccess{PubKey: pubkey}
	if t == nil || !t.PrivateServiceEnabled() {
		return access, nil
	}
	if pubkey == "" {
		return access, errors.New("auth-required: private service requires NIP-42 or NIP-98")
	}
	banned, err := t.community.IsBanned(ctx, pubkey)
	if err != nil {
		return access, err
	}
	if banned {
		return access, errors.New("blocked: private service identity is banned")
	}
	role, err := t.community.Role(ctx, pubkey)
	if err != nil {
		return access, err
	}
	access.Role = role
	access.Owner = role == "owner" || pubkey == t.Policy().Owner
	access.Member = role == "member" || role == "moderator"
	if !access.Allowed() {
		return access, errors.New("restricted: private service membership required")
	}
	return access, nil
}

func (t *Tenant) requirePrivateAccess(ctx context.Context, pubkey string) error {
	_, err := t.PrivateAccess(ctx, pubkey)
	return err
}

// AuthorizePrivatePeer is the narrow boundary for outbound Git/event sync.
// Callers must invoke it immediately before using a private peer credential;
// a previously successful check must never be reused after membership changes.
func (t *Tenant) AuthorizePrivatePeer(ctx context.Context, pubkey string) error {
	if !t.PrivateServiceEnabled() {
		return errors.New("restricted: private peer access is disabled")
	}
	return t.requirePrivateAccess(ctx, pubkey)
}

// privateNIP11 returns the metadata safe to expose before authentication.
// The advertised owner key is the service identity needed by NIP-42 clients;
// tenant-specific descriptive fields remain intentionally omitted.
func (t *Tenant) privateNIP11() map[string]any {
	profile := t.PrivateProfile()
	return map[string]any{
		"name":             "private relay",
		"description":      "authenticated private relay",
		"pubkey":           profile.Owner,
		"supported_grasps": []string{"GRASP-01", "GRASP-08"},
		"private_service":  true,
	}
}

func privatePolicy(p policy.Policy) bool {
	return p.PrivateServiceEnabled()
}

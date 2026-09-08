package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

func TestPrivateProfileRedactsTenantMetadata(t *testing.T) {
	tenant := &Tenant{policy: policy.Defaults(strings.Repeat("a", 64))}
	tenant.policy.Features.Grasp = true
	tenant.policy.Features.Grasp08 = true
	tenant.policy.Reads = "members"
	tenant.policy.Name = "secret-project"
	tenant.policy.Description = "do not expose"
	profile := tenant.PrivateProfile()
	if !profile.Enabled || profile.Protocol != "GRASP-08" || profile.Reads != "members" {
		t.Fatalf("unexpected profile: %#v", profile)
	}
	if len(profile.Authentication) != 2 || profile.Authentication[0] != "NIP-42" || profile.Authentication[1] != "NIP-98" {
		t.Fatalf("unexpected auth methods: %#v", profile.Authentication)
	}
	info := tenant.privateNIP11()
	if info["name"] == tenant.policy.Name || info["description"] == tenant.policy.Description {
		t.Fatalf("private NIP-11 leaked tenant metadata: %#v", info)
	}
	if info["private_service"] != true {
		t.Fatalf("private service marker missing: %#v", info)
	}
}

func TestPrivateAccessDisabledIsNoop(t *testing.T) {
	tenant := &Tenant{policy: policy.Defaults(strings.Repeat("a", 64))}
	access, err := tenant.PrivateAccess(context.Background(), "outsider")
	if err != nil || access.Allowed() {
		t.Fatalf("disabled private access = %#v, %v", access, err)
	}
}

func TestPrivatePolicyShape(t *testing.T) {
	p := policy.Defaults(strings.Repeat("a", 64))
	p.Features.Grasp08 = true
	p.Features.Grasp = true
	if privatePolicy(p) {
		t.Fatal("open private policy recognized")
	}
	p.Reads = "members"
	if !privatePolicy(p) {
		t.Fatal("members-only GRASP-08 policy not recognized")
	}
}

func TestPrivateAccessFollowsMembershipRevocation(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	p := tenant.Policy()
	p.Features.Grasp = true
	p.Features.Grasp08 = true
	p.Reads = "members"
	if err := tenant.applyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	memberSecret := strings.Repeat("2", 64)
	member, err := event.PublicKey(memberSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.community.Execute(ctx, p.Owner, "setmember", []json.RawMessage{json.RawMessage(`"` + member + `"`), json.RawMessage(`{"name":"reader"}`)}); err != nil {
		t.Fatal(err)
	}
	access, err := tenant.PrivateAccess(ctx, member)
	if err != nil || !access.Allowed() || !access.Member || access.Owner {
		t.Fatalf("member access = %#v, %v", access, err)
	}
	if _, err := tenant.community.Execute(ctx, p.Owner, "removemember", []json.RawMessage{json.RawMessage(`"` + member + `"`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.PrivateAccess(ctx, member); err == nil {
		t.Fatal("revoked member retained private access")
	}
}

func TestPrivateNIP11OmitsTenantPresentation(t *testing.T) {
	_, tenant := testTenant(t)
	p := tenant.Policy()
	p.Features.Grasp = true
	p.Features.Grasp08 = true
	p.Reads = "members"
	p.Name = "secret-name"
	p.Description = "secret-description"
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://relay.test/", nil)
	req.Header.Set("Accept", "application/nostr+json")
	res := httptest.NewRecorder()
	tenant.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("NIP-11 status=%d body=%s", res.Code, res.Body.String())
	}
	body := res.Body.String()
	if strings.Contains(body, "secret-name") || strings.Contains(body, "secret-description") {
		t.Fatalf("private NIP-11 leaked presentation metadata: %s", body)
	}
	if !strings.Contains(body, "GRASP-08") || !strings.Contains(body, "private_service") {
		t.Fatalf("private NIP-11 omitted service profile: %s", body)
	}
	nip05 := httptest.NewRequest(http.MethodGet, "http://relay.test/.well-known/nostr.json?name=reader", nil)
	nip05Result := httptest.NewRecorder()
	tenant.ServeHTTP(nip05Result, nip05)
	if nip05Result.Code != http.StatusUnauthorized {
		t.Fatalf("private NIP-05 exposed unsigned metadata: %d %s", nip05Result.Code, nip05Result.Body.String())
	}
}

func TestPrivateGitRejectsSignedOutsiderBeforeRepositoryLookup(t *testing.T) {
	_, tenant := testTenant(t)
	p := tenant.Policy()
	p.Features.Grasp = true
	p.Features.Grasp08 = true
	p.Reads = "members"
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	path := "/npub1unknown/private.git/info/refs?service=git-upload-pack"
	outsider := httptest.NewRequest(http.MethodGet, "http://relay.test"+path, nil)
	signRequestWithSecret(t, outsider, "", strings.Repeat("3", 64))
	denied := httptest.NewRecorder()
	tenant.ServeHTTP(denied, outsider)
	if denied.Code != http.StatusUnauthorized || denied.Body.Len() != 0 || denied.Header().Get("WWW-Authenticate") != `Nostr method="GET"` {
		t.Fatalf("signed outsider reached private Git lookup: %d %s", denied.Code, denied.Body.String())
	}
	owner := httptest.NewRequest(http.MethodGet, "http://relay.test"+path, nil)
	signRequest(t, owner, "")
	allowed := httptest.NewRecorder()
	tenant.ServeHTTP(allowed, owner)
	if allowed.Code == http.StatusForbidden || allowed.Code == http.StatusUnauthorized {
		t.Fatalf("owner was denied before repository lookup: %d %s", allowed.Code, allowed.Body.String())
	}
}

func signRequestWithSecret(t *testing.T, r *http.Request, body, secret string) {
	t.Helper()
	hash := sha256.Sum256([]byte(body))
	tags := [][]string{{"u", r.URL.String()}, {"method", r.Method}, {"payload", hex.EncodeToString(hash[:])}}
	if i := strings.Index(r.URL.Path, ".git/"); i >= 0 {
		tags = [][]string{{"u", r.URL.Scheme + "://" + r.URL.Host + r.URL.Path[:i+4]}, {"method", http.MethodGet}}
	}
	e := event.Event{Kind: 27235, CreatedAt: time.Now().Unix(), Content: "", Tags: tags}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Nostr "+base64.StdEncoding.EncodeToString(raw))
}

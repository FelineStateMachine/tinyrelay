package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestPrivatePeerGitAndMetadataSyncAcrossTenants(t *testing.T) {
	ctx := context.Background()
	sourceSecret := strings.Repeat("1", 64)
	destSecret := strings.Repeat("2", 64)
	sourceOwner, err := event.PublicKey(sourceSecret)
	if err != nil {
		t.Fatal(err)
	}
	destOwner, err := event.PublicKey(destSecret)
	if err != nil {
		t.Fatal(err)
	}
	_, source, sourceServer := privatePeerTenant(t, "source", sourceOwner)
	_, dest, destServer := privatePeerTenant(t, "dest", destOwner)
	configurePrivatePeer(t, source, sourceOwner, dest.records.PublicKey(), destServer.URL)
	configurePrivatePeer(t, dest, destOwner, source.records.PublicKey(), sourceServer.URL)
	if _, err := source.community.Execute(ctx, sourceOwner, "setmember", []json.RawMessage{json.RawMessage(fmt.Sprintf(`%q`, destOwner)), json.RawMessage(`{"name":"peer-owner"}`)}); err != nil {
		t.Fatal(err)
	}

	identifier := "private"
	repo, announcement, state := seedPrivateRepository(t, dest, destOwner, destSecret, identifier, destServer.URL)
	probe, err := destServer.Client().Get(repo.Clone[0] + "/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	probe.Body.Close()
	if probe.StatusCode != 401 {
		t.Fatalf("private clone probe status=%d url=%s", probe.StatusCode, repo.Clone[0])
	}
	authHeader, err := source.privateHTTPAuth(ctx, "GET", repo.Clone[0]+"/info/refs?service=git-upload-pack", "")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, repo.Clone[0]+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", authHeader)
	checked, err := destServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	checked.Body.Close()
	if checked.StatusCode != 200 {
		t.Fatalf("authenticated clone probe status=%d url=%s owner=%s", checked.StatusCode, repo.Clone[0], repo.Owner)
	}
	if err := source.gitSync(ctx, repo); err != nil {
		t.Fatalf("private Git sync: %v", err)
	}
	var untrustedAuth string
	untrustedPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		untrustedAuth = r.Header.Get("Authorization")
		http.NotFound(w, r)
	}))
	t.Cleanup(untrustedPeer.Close)
	publicRepo := repo
	// Use an empty local repository so this actually exercises the peer path;
	// complete local objects intentionally need no network request.
	publicRepo.Identifier = "unconfigured-peer"
	publicRepo.Clone = []string{untrustedPeer.URL + "/private.git"}
	if err := source.git.FetchMissing(ctx, publicRepo, publicRepo.Clone, publicRepo.Refs); err == nil {
		t.Fatal("unconfigured public peer unexpectedly supplied private Git objects")
	}
	if untrustedAuth != "" {
		t.Fatalf("unconfigured public peer received credentials: %q", untrustedAuth)
	}
	if !gitObjectExists(t, source.meta.Paths.Git, repo, repo.Refs["refs/heads/main"]) {
		t.Fatal("source tenant did not receive the signed Git object")
	}
	if err := source.gitEventSync(ctx, repo); err != nil {
		t.Fatalf("private metadata sync: %v", err)
	}
	for _, item := range []event.Event{announcement} {
		rows, err := source.store.Query(ctx, event.Filter{IDs: []string{item.ID}, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
		if err != nil || len(rows.Events) != 1 {
			t.Fatalf("metadata event %s missing after sync: %v", item.ID, err)
		}
	}
	var rawStateID string
	if err := source.store.DB().QueryRowContext(ctx, "SELECT id FROM events WHERE kind=30618 AND pubkey=? AND d=?", state.PubKey, identifier).Scan(&rawStateID); err != nil || rawStateID != state.ID {
		t.Fatalf("state metadata was not persisted: id=%q err=%v", rawStateID, err)
	}

	if err := source.git.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if !gitObjectExists(t, source.meta.Paths.Git, repo, repo.Refs["refs/heads/main"]) {
		t.Fatal("source Git object disappeared after reload")
	}
	var publicAuth string
	guardPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publicAuth = r.Header.Get("Authorization")
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "application/nostr+json")
			_, _ = w.Write([]byte(`{"supported_grasps":["GRASP-08"]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(guardPeer.Close)
	publicPolicy := source.Policy()
	publicPolicy.Features.Grasp08 = false
	publicPolicy.Reads = "open"
	publicPolicy.PrivatePeers = []string{guardPeer.URL}
	if err := source.applyPolicy(ctx, publicPolicy); err == nil {
		t.Fatal("private tenant policy was made public while private repository metadata existed")
	}
	_, publicTenant := testTenant(t)
	publicPolicy = publicTenant.Policy()
	publicPolicy.Features.Grasp = true
	publicPolicy.PrivatePeers = []string{guardPeer.URL}
	if err := publicTenant.applyPolicy(ctx, publicPolicy); err != nil {
		t.Fatal(err)
	}
	publicCheckRepo := repo
	publicCheckRepo.Identifier = "public-profile-peer"
	publicCheckRepo.Clone = []string{guardPeer.URL + "/private.git"}
	if err := publicTenant.gitSync(ctx, publicCheckRepo); err == nil {
		t.Fatal("public profile unexpectedly fetched from a private peer")
	}
	if publicAuth != "" {
		t.Fatalf("public profile sent private peer credentials: %q", publicAuth)
	}
}

func privatePeerTenant(t *testing.T, name, owner string) (*App, *Tenant, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	app, err := New(ctx, Config{DataDir: t.TempDir(), DefaultTenant: name, AllowPrivateRelays: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close(ctx) })
	meta, err := app.Create(ctx, CreateOptions{Name: name, Owner: owner, Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { app.ServeHTTP(w, r) }))
	t.Cleanup(server.Close)
	tenant, err := app.tenant(ctx, meta, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := tenant.Policy()
	p.Features.Grasp = true
	p.Features.Grasp02 = true
	p.Features.Grasp03 = true
	p.Features.Grasp05 = true
	p.Features.Grasp08 = true
	p.Reads = "members"
	if err := tenant.applyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	return app, tenant, server
}

func configurePrivatePeer(t *testing.T, tenant *Tenant, owner, member, peer string) {
	t.Helper()
	if _, err := tenant.community.Execute(context.Background(), owner, "setmember", []json.RawMessage{json.RawMessage(fmt.Sprintf(`%q`, member)), json.RawMessage(`{"name":"peer"}`)}); err != nil {
		t.Fatal(err)
	}
	p := tenant.Policy()
	p.PrivatePeers = []string{peer}
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func seedPrivateRepository(t *testing.T, tenant *Tenant, owner, secret, identifier, baseURL string) (gitrelay.Repository, event.Event, event.Event) {
	t.Helper()
	path := filepath.Join(tenant.meta.Paths.Git, owner, identifier+".git")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	peerGitRun(t, "init", "--bare", path)
	work := filepath.Join(t.TempDir(), "work")
	peerGitRun(t, "init", work)
	peerGitRun(t, "-C", work, "config", "user.email", "peer@example.test")
	peerGitRun(t, "-C", work, "config", "user.name", "Peer")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("private peer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "medium.bin"), bytesOf(128<<10), 0600); err != nil {
		t.Fatal(err)
	}
	peerGitRun(t, "-C", work, "add", ".")
	peerGitRun(t, "-C", work, "commit", "-m", "private peer")
	peerGitRun(t, "-C", work, "branch", "-M", "main")
	sha := strings.TrimSpace(peerGitOutput(t, "-C", work, "rev-parse", "HEAD"))
	peerGitRun(t, "-C", work, "push", path, "HEAD:refs/heads/main")
	ownerNPub := testNpub(owner)
	cloneURL := baseURL + "/" + ownerNPub + "/" + identifier + ".git"
	announcement := event.Event{Kind: event.KIND_REPO, CreatedAt: time.Now().Unix(), Tags: [][]string{{"d", identifier}, {"clone", cloneURL}, {"relays", tenant.RelayURL()}}}
	if err := event.Sign(&announcement, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.router.Publish(context.Background(), announcement, relay.Session{PubKeys: []string{owner}}); err != nil {
		t.Fatal(err)
	}
	storedAnnouncement, err := tenant.store.Query(context.Background(), event.Filter{IDs: []string{announcement.ID}, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
	if err != nil || len(storedAnnouncement.Events) != 1 {
		t.Fatalf("destination announcement was not stored: %v", err)
	}
	state := event.Event{Kind: event.KIND_REPO_STATE, CreatedAt: time.Now().Unix(), Tags: [][]string{{"d", identifier}, {"HEAD", "ref: refs/heads/main"}, {"refs/heads/main", sha}}}
	if err := event.Sign(&state, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.router.Publish(context.Background(), state, relay.Session{PubKeys: []string{owner}}); err != nil {
		t.Fatal(err)
	}
	repo := gitrelay.Repository{Owner: owner, Identifier: identifier, EventID: announcement.ID, Clone: []string{cloneURL}, Relays: []string{tenant.RelayURL()}, Refs: map[string]string{"refs/heads/main": sha}, Head: "ref: refs/heads/main"}
	if err := tenant.git.CommitAfterStore(context.Background(), announcement, repo); err != nil {
		t.Fatal(err)
	}
	if err := tenant.git.CommitAfterStore(context.Background(), state, repo); err != nil {
		t.Fatal(err)
	}
	return repo, announcement, state
}

func gitObjectExists(t *testing.T, root string, repo gitrelay.Repository, object string) bool {
	t.Helper()
	path := filepath.Join(root, repo.Owner, repo.Identifier+".git")
	cmd := exec.Command("git", "--git-dir", path, "cat-file", "-e", object)
	return cmd.Run() == nil
}

func peerGitRun(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
func peerGitOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
func bytesOf(n int) []byte { return make([]byte, n) }

func testNpub(hexKey string) string {
	data := make([]byte, len(hexKey)/2)
	for i := range data {
		fmt.Sscanf(hexKey[i*2:i*2+2], "%02x", &data[i])
	}
	converted := convertBits(data, 8, 5, true)
	charset := "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
	values := converted
	values = append(values, bech32Checksum("npub", values)...)
	out := "npub1"
	for _, value := range values {
		out += string(charset[value])
	}
	return out
}

func convertBits(data []byte, from, to uint, pad bool) []byte {
	acc, bits := 0, uint(0)
	max := (1 << to) - 1
	out := make([]byte, 0, len(data)*int(from)/int(to)+1)
	for _, value := range data {
		acc = (acc << from) | int(value)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte((acc>>bits)&max))
		}
	}
	if pad && bits > 0 {
		out = append(out, byte((acc<<(to-bits))&max))
	}
	return out
}

func bech32Checksum(hrp string, data []byte) []byte {
	values := make([]int, 0, len(hrp)*2+len(data)+6)
	for _, char := range hrp {
		values = append(values, int(char>>5))
	}
	values = append(values, 0)
	for _, char := range hrp {
		values = append(values, int(char&31))
	}
	for _, value := range data {
		values = append(values, int(value))
	}
	values = append(values, 0, 0, 0, 0, 0, 0)
	polymod := bech32Polymod(values) ^ 1
	checksum := make([]byte, 6)
	for i := range checksum {
		checksum[i] = byte((polymod >> uint(5*(5-i))) & 31)
	}
	return checksum
}

func bech32Polymod(values []int) uint32 {
	generators := [...]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	value := uint32(1)
	for _, item := range values {
		top := value >> 25
		value = (value&0x1ffffff)<<5 ^ uint32(item)
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 != 0 {
				value ^= generators[i]
			}
		}
	}
	return value
}

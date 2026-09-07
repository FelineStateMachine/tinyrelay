package daemon

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"net/http/httptest"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
)

func TestImmediateMultiRefPushAfterSignedState(t *testing.T) {
	ctx := context.Background()
	secret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	app, tenant := testTenant(t)
	p := tenant.Policy()
	p.Features.Grasp = true
	if err := tenant.applyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}

	announcement := event.Event{Kind: event.KIND_REPO, CreatedAt: time.Now().Unix(), Tags: [][]string{{"d", "many"}, {"clone", "http://relay.test/" + "npub10xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqpkge6d/many.git"}, {"relays", "ws://relay.test"}}}
	if err := event.Sign(&announcement, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.router.Publish(ctx, announcement, relay.Session{PubKeys: []string{owner}}); err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(t.TempDir(), "source.git")
	gitTest(t, "init", "--bare", source)
	work := filepath.Join(t.TempDir(), "work")
	gitTest(t, "clone", source, work)
	gitTest(t, "-C", work, "config", "user.email", "test@example.com")
	gitTest(t, "-C", work, "config", "user.name", "Test")
	if err := osWrite(filepath.Join(work, "README"), []byte("multi\n")); err != nil {
		t.Fatal(err)
	}
	gitTest(t, "-C", work, "add", "README")
	gitTest(t, "-C", work, "commit", "-m", "multi")
	gitTest(t, "-C", work, "branch", "-M", "main")
	sha := strings.TrimSpace(gitOutput(t, "-C", work, "rev-parse", "HEAD"))
	refs := [][]string{{"HEAD", "ref: refs/heads/main"}}
	for i := 0; i < 30; i++ {
		name := "refs/heads/branch-" + strconv.Itoa(i)
		if i == 0 {
			name = "refs/heads/main"
		}
		gitTest(t, "-C", work, "update-ref", name, sha)
		refs = append(refs, []string{name, sha})
	}
	state := event.Event{Kind: event.KIND_REPO_STATE, CreatedAt: time.Now().Unix(), Tags: append([][]string{{"d", "many"}}, refs...)}
	if err := event.Sign(&state, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.router.Publish(ctx, state, relay.Session{PubKeys: []string{owner}}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(app)
	defer server.Close()
	const ownerNPub = "npub10xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqpkge6d"
	url := server.URL + "/r/main/" + ownerNPub + "/many.git"
	gitTest(t, "-C", work, "push", url, "refs/heads/*:refs/heads/*")
	// Existing repositories must obey live hosting/read policy changes too.
	checkAdvertisement := func(want int) {
		t.Helper()
		res, err := server.Client().Get(url + "/info/refs?service=git-upload-pack")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
		if res.StatusCode != want {
			t.Fatalf("Git policy response = %d, want %d", res.StatusCode, want)
		}
	}
	p = tenant.Policy()
	p.Reads = "members"
	if err := tenant.applyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	checkAdvertisement(http.StatusForbidden)
	p.Reads = "open"
	p.Features.Grasp = false
	if err := tenant.applyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	checkAdvertisement(http.StatusNotFound)
	p.Features.Grasp = true
	if err := tenant.applyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	checkAdvertisement(http.StatusOK)
}

func gitTest(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func gitOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func osWrite(path string, data []byte) error {
	return os.WriteFile(path, data, 0600)
}

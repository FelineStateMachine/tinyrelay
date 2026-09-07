package daemon

import (
	"context"
	"crypto/rand"
	"io"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
)

// A reverse proxy without full duplex HTTP/1.1 discards the unread request
// body as soon as the backend starts responding. git-http-backend writes its
// headers before it reads the pack, so the relay must hold the response until
// git produces body bytes or a large push arrives truncated at the proxy.
func TestLargePushSurvivesHalfDuplexReverseProxy(t *testing.T) {
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
	const ownerNPub = "npub10xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqpkge6d"
	announcement := event.Event{Kind: event.KIND_REPO, CreatedAt: time.Now().Unix(), Tags: [][]string{{"d", "big"}, {"clone", "http://relay.test/" + ownerNPub + "/big.git"}, {"relays", "ws://relay.test"}}}
	if err := event.Sign(&announcement, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.router.Publish(ctx, announcement, relay.Session{PubKeys: []string{owner}}); err != nil {
		t.Fatal(err)
	}

	work := filepath.Join(t.TempDir(), "work")
	gitTest(t, "init", "-b", "main", work)
	gitTest(t, "-C", work, "config", "user.email", "test@example.com")
	gitTest(t, "-C", work, "config", "user.name", "Test")
	blob := make([]byte, 1<<20)
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "blob.bin"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, "-C", work, "add", "blob.bin")
	gitTest(t, "-C", work, "commit", "-m", "big")
	sha := strings.TrimSpace(gitOutput(t, "-C", work, "rev-parse", "HEAD"))
	state := event.Event{Kind: event.KIND_REPO_STATE, CreatedAt: time.Now().Unix(), Tags: [][]string{{"d", "big"}, {"HEAD", "ref: refs/heads/main"}, {"refs/heads/main", sha}}}
	if err := event.Sign(&state, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.router.Publish(ctx, state, relay.Session{PubKeys: []string{owner}}); err != nil {
		t.Fatal(err)
	}

	backend := httptest.NewServer(app)
	defer backend.Close()
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Throttle the upload so the backend's headers arrive while the pack is
	// still streaming, as they do over a real network.
	proxy := httptest.NewServer(&httputil.ReverseProxy{Rewrite: func(request *httputil.ProxyRequest) {
		request.SetURL(target)
		request.Out.Body = &slowBody{ReadCloser: request.In.Body}
	}})
	defer proxy.Close()

	// A small post buffer makes git stream the pack as a chunked HTTP/1.1 body.
	gitTest(t, "-C", work, "-c", "http.postBuffer=1024", "-c", "http.version=HTTP/1.1", "push", proxy.URL+"/r/main/"+ownerNPub+"/big.git", "refs/heads/main:refs/heads/main")
	clone := filepath.Join(t.TempDir(), "clone")
	gitTest(t, "clone", proxy.URL+"/r/main/"+ownerNPub+"/big.git", clone)
	if got := strings.TrimSpace(gitOutput(t, "-C", clone, "rev-parse", "HEAD")); got != sha {
		t.Fatalf("clone HEAD = %s, want %s", got, sha)
	}
}

type slowBody struct{ io.ReadCloser }

func (b *slowBody) Read(p []byte) (int, error) {
	if len(p) > 4096 {
		p = p[:4096]
	}
	time.Sleep(2 * time.Millisecond)
	return b.ReadCloser.Read(p)
}

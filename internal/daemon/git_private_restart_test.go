package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
)

func TestPrivateSignedGitStateRemainsHiddenAfterRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	secret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(ctx, Config{DataDir: dir, DefaultTenant: "main"})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := app.Create(ctx, CreateOptions{Name: "main", Owner: owner, Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := app.tenant(ctx, meta, "http://relay.test")
	if err != nil {
		t.Fatal(err)
	}
	p := tenant.Policy()
	p.Features.Grasp = true
	if err := tenant.applyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	announcement := event.Event{Kind: event.KIND_REPO, CreatedAt: 1, Tags: [][]string{{"d", "private"}, {"private", "true"}, {"clone", "http://relay.test/" + "npub10xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqpkge6d/private.git"}, {"relays", "ws://relay.test"}}}
	if err := event.Sign(&announcement, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.router.Publish(ctx, announcement, relay.Session{PubKeys: []string{owner}}); err != nil {
		t.Fatal(err)
	}
	state := event.Event{Kind: event.KIND_REPO_STATE, CreatedAt: 2, Tags: [][]string{{"d", "private"}, {"HEAD", "ref: refs/heads/main"}, {"refs/heads/main", strings.Repeat("a", 40)}}}
	if err := event.Sign(&state, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.router.Publish(ctx, state, relay.Session{PubKeys: []string{owner}}); err != nil {
		t.Fatal(err)
	}
	if !tenant.git.IsPending(state.ID) {
		t.Fatal("missing-object state was not held pending")
	}
	if err := app.Close(ctx); err != nil {
		t.Fatal(err)
	}

	app, err = New(ctx, Config{DataDir: dir, DefaultTenant: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(ctx, "http://relay.test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close(ctx) })
	restarted := app.tenants[meta.ID]
	if restarted == nil || !restarted.git.IsPending(state.ID) {
		t.Fatal("restart did not restore pending signed state")
	}
	path := "/npub10xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqpkge6d/private.git/info/refs?service=git-upload-pack"
	req := httptest.NewRequest(http.MethodGet, "http://relay.test/r/main"+path, nil)
	denied := httptest.NewRecorder()
	app.ServeHTTP(denied, req)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("unsigned private Git read status=%d", denied.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "http://relay.test/r/main"+path, nil)
	signRequest(t, req, "")
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "# service=git-upload-pack") {
		t.Fatalf("authorized private Git read after restart status = %d, body=%q", res.Code, res.Body.String())
	}
	pushPath := "/npub10xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqpkge6d/private.git/git-receive-pack"
	body := "0000"
	push := httptest.NewRequest(http.MethodPost, "http://relay.test/r/main"+pushPath, strings.NewReader(body))
	push.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	signRequest(t, push, body)
	pushResult := httptest.NewRecorder()
	app.ServeHTTP(pushResult, push)
	if pushResult.Code != http.StatusOK || pushResult.Header().Get("Content-Type") != "application/x-git-receive-pack-result" {
		t.Fatalf("signed private Git negotiation failed: %d %q", pushResult.Code, pushResult.Body.String())
	}
	tampered := httptest.NewRequest(http.MethodPost, "http://relay.test/r/main"+pushPath, strings.NewReader("0002"))
	tampered.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	signRequest(t, tampered, "0001")
	rejected := httptest.NewRecorder()
	app.ServeHTTP(rejected, tampered)
	if rejected.Code != http.StatusForbidden || !strings.Contains(rejected.Body.String(), "payload hash") {
		t.Fatalf("tampered Git payload reached protocol handling: %d %q", rejected.Code, rejected.Body.String())
	}
}

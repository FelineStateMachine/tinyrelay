package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/views"
)

func TestCustomViewArtifactSourceVisibilityCoversMediaAndEventReads(t *testing.T) {
	tenant, server, _ := viewTenant(t, []int{1}, nil)
	server.answer(http.StatusOK, svgArtifact(0))
	first := signedEvent(t, testOwnerSecret, 1, time.Now().Unix(), nil, "```mermaid\ngraph TD; a-->b;\n```")
	if err := publishAs(t, tenant, first); err != nil {
		t.Fatal(err)
	}
	runPendingView(t, tenant, "diagrams", first.ID)
	second := signedEvent(t, testOwnerSecret, 1, time.Now().Unix()+1, nil, first.Content)
	if err := publishAs(t, tenant, second); err != nil {
		t.Fatal(err)
	}
	runPendingView(t, tenant, "diagrams", second.ID)
	ctx := context.Background()
	if _, err := tenant.store.DB().ExecContext(ctx, `INSERT INTO hidden_events(id,reason) VALUES(?,'test')`, first.ID); err != nil {
		t.Fatal(err)
	}
	_, image, ok := strings.Cut(views.Figure("diagrams", "mermaid", "graph TD; a-->b;"), `<img src="`)
	if !ok {
		t.Fatal("custom view has no image")
	}
	path, _, _ := strings.Cut(image, `"`)
	if response := getArtifact(t, tenant, path, ""); response.Code != http.StatusOK {
		t.Fatalf("visible source should serve shared media: %d", response.Code)
	}
	hash := views.Hash("mermaid", "graph TD; a-->b;")
	rows, err := tenant.Query(ctx, []event.Filter{{Kinds: []int{event.KIND_VIEW}, Tags: map[string][]string{"d": {"bind.ws/view/diagrams/" + hash}}}}, relay.Session{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("artifact event leaked a hidden source: %d", len(rows))
	}
}

func TestCustomViewArtifactSourceLifecycleBlocksMediaAndEventReads(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(context.Context, *Tenant, event.Event) error
	}{
		{name: "hidden", mutate: func(ctx context.Context, tenant *Tenant, source event.Event) error {
			_, err := tenant.store.DB().ExecContext(ctx, `INSERT INTO hidden_events(id,reason) VALUES(?,'test')`, source.ID)
			return err
		}},
		{name: "deleted", mutate: func(ctx context.Context, tenant *Tenant, source event.Event) error {
			_, err := tenant.store.DB().ExecContext(ctx, `DELETE FROM events WHERE id=?`, source.ID)
			return err
		}},
		{name: "expired", mutate: func(ctx context.Context, tenant *Tenant, source event.Event) error {
			_, err := tenant.store.DB().ExecContext(ctx, `UPDATE events SET expires=? WHERE id=?`, time.Now().Unix()-1, source.ID)
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tenant, server, _ := viewTenant(t, []int{1}, nil)
			server.answer(http.StatusOK, svgArtifact(0))
			source := signedEvent(t, testOwnerSecret, 1, time.Now().Unix(), nil, "```mermaid\nA\n```")
			if err := publishAs(t, tenant, source); err != nil {
				t.Fatal(err)
			}
			runPendingView(t, tenant, "diagrams", source.ID)
			hash := views.Hash("mermaid", "A")
			path := "/views/diagrams/" + hash + ".svg"
			if response := getArtifact(t, tenant, path, ""); response.Code != http.StatusOK {
				t.Fatalf("baseline artifact: %d", response.Code)
			}
			if err := tc.mutate(context.Background(), tenant, source); err != nil {
				t.Fatal(err)
			}
			if response := getArtifact(t, tenant, path, ""); response.Code != http.StatusNotFound {
				t.Fatalf("inaccessible source served media: %d", response.Code)
			}
			rows, err := tenant.Query(context.Background(), []event.Filter{{Kinds: []int{event.KIND_VIEW}, Tags: map[string][]string{"d": {"bind.ws/view/diagrams/" + hash}}}}, relay.Session{})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 {
				t.Fatalf("inaccessible source leaked artifact event: %d", len(rows))
			}
		})
	}
}

func TestCustomViewDoesNotTransformExpiredQueuedSource(t *testing.T) {
	tenant, server, _ := viewTenant(t, []int{1}, nil)
	server.answer(http.StatusOK, svgArtifact(0))
	now := time.Now().Unix()
	source := signedEvent(t, testOwnerSecret, 1, now, [][]string{{"expiration", fmt.Sprint(now + 3600)}}, "```mermaid\nA\n```")
	if err := publishAs(t, tenant, source); err != nil {
		t.Fatal(err)
	}
	queued := pendingViewIntents(t, tenant, "diagrams", source.ID)
	if len(queued) != 1 {
		t.Fatalf("queued intents = %d", len(queued))
	}
	if _, err := tenant.store.DB().ExecContext(context.Background(), `UPDATE events SET expires=? WHERE id=?`, now-1, source.ID); err != nil {
		t.Fatal(err)
	}
	runViewIntent(t, tenant, queued[0])
	if server.count() != 0 {
		t.Fatalf("expired source was sent to transform: %d requests", server.count())
	}
}

func TestCustomViewArtifactCoordinateRequiresCurrentMatchingSource(t *testing.T) {
	tenant, server, _ := viewTenant(t, []int{event.KIND_REPO_STATE}, nil)
	ctx := context.Background()
	p := tenant.Policy()
	p.Features.Grasp = true
	if err := tenant.applyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	announcement := signedEvent(t, testOwnerSecret, event.KIND_REPO, time.Now().Unix(), [][]string{{"d", "docs"}, {"clone", "http://127.0.0.1:8080/npub10xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqpkge6d/docs.git"}, {"relays", "ws://127.0.0.1:8080"}}, "")
	if err := publishAs(t, tenant, announcement); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.git")
	gitTest(t, "init", "--bare", source)
	work := filepath.Join(t.TempDir(), "work")
	gitTest(t, "clone", source, work)
	gitTest(t, "-C", work, "config", "user.email", "test@example.com")
	gitTest(t, "-C", work, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# Docs\n\n```mermaid\ngraph LR; a-->b;\n```\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, "-C", work, "add", "README.md")
	gitTest(t, "-C", work, "commit", "-m", "readme")
	gitTest(t, "-C", work, "branch", "-M", "main")
	sha := strings.TrimSpace(gitOutput(t, "-C", work, "rev-parse", "HEAD"))
	state := signedEvent(t, testOwnerSecret, event.KIND_REPO_STATE, time.Now().Unix(), [][]string{{"d", "docs"}, {"HEAD", "ref: refs/heads/main"}, {"refs/heads/main", sha}}, "")
	if err := publishAs(t, tenant, state); err != nil {
		t.Fatal(err)
	}
	relayServer := httptest.NewServer(tenant)
	defer relayServer.Close()
	gitTest(t, "-C", work, "push", relayServer.URL+"/npub10xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqpkge6d/docs.git", "refs/heads/main:refs/heads/main")
	queued := pendingViewIntents(t, tenant, "diagrams", state.ID)
	if len(queued) != 1 {
		t.Fatalf("state intents = %d", len(queued))
	}
	server.answer(http.StatusOK, svgArtifact(0))
	runViewIntent(t, tenant, queued[0])
	hash := views.Hash("mermaid", "graph LR; a-->b;")
	path := "/views/diagrams/" + hash + ".svg"
	if blocks, blockErr := tenant.customViews.sourceBlocks(ctx, state); blockErr != nil {
		t.Fatalf("initial source blocks: %v", blockErr)
	} else if len(blocks) != 1 {
		t.Fatalf("initial source blocks = %+v", blocks)
	}
	if response := getArtifact(t, tenant, path, ""); response.Code != http.StatusOK {
		t.Fatalf("artifact through current state: %d", response.Code)
	}
	rows, err := tenant.Query(ctx, []event.Filter{{Kinds: []int{event.KIND_VIEW}, Tags: map[string][]string{"d": {"bind.ws/view/diagrams/" + hash}}}}, relay.Session{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("artifact query through current state = %d, want 1", len(rows))
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# Docs\n\nNo diagrams here.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, "-C", work, "add", "README.md")
	gitTest(t, "-C", work, "commit", "-m", "remove diagram")
	newSHA := strings.TrimSpace(gitOutput(t, "-C", work, "rev-parse", "HEAD"))
	replacement := signedEvent(t, testOwnerSecret, event.KIND_REPO_STATE, time.Now().Unix()+1, [][]string{{"d", "docs"}, {"HEAD", "ref: refs/heads/main"}, {"refs/heads/main", newSHA}}, "")
	if err := publishAs(t, tenant, replacement); err != nil {
		t.Fatal(err)
	}
	gitTest(t, "-C", work, "push", relayServer.URL+"/npub10xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqpkge6d/docs.git", "refs/heads/main:refs/heads/main")
	if _, ok := storedArtifact(t, tenant, "diagrams", hash); !ok {
		t.Fatal("replacement removed the artifact before the access check")
	}
	if response := getArtifact(t, tenant, path, ""); response.Code != http.StatusNotFound {
		t.Fatalf("stale coordinate served media: %d", response.Code)
	}
	rows, err = tenant.Query(ctx, []event.Filter{{Kinds: []int{event.KIND_VIEW}, Tags: map[string][]string{"d": {"bind.ws/view/diagrams/" + hash}}}}, relay.Session{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("stale coordinate leaked artifact event: %d", len(rows))
	}
}

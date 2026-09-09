package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
)

const testOwnerNPub = "npub10xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqpkge6d"

// agentTestRepo announces one repository from the test owner and returns a
// working clone with one commit on main, plus that commit's id.
func agentTestRepo(t *testing.T, tenant *Tenant, identifier string) (string, string) {
	t.Helper()
	now := time.Now().Unix()
	announcement := signedEvent(t, testOwnerSecret, event.KIND_REPO, now, [][]string{{"d", identifier}, {"clone", "http://relay.test/" + testOwnerNPub + "/" + identifier + ".git"}, {"relays", "ws://relay.test"}}, "")
	if err := publishAs(t, tenant, announcement); err != nil {
		t.Fatalf("announcement rejected: %v", err)
	}
	source := filepath.Join(t.TempDir(), "source.git")
	gitTest(t, "init", "--bare", source)
	work := filepath.Join(t.TempDir(), "work")
	gitTest(t, "clone", source, work)
	gitTest(t, "-C", work, "config", "user.email", "test@example.com")
	gitTest(t, "-C", work, "config", "user.name", "Test")
	if err := osWrite(filepath.Join(work, "README"), []byte("agent\n")); err != nil {
		t.Fatal(err)
	}
	gitTest(t, "-C", work, "add", "README")
	gitTest(t, "-C", work, "commit", "-m", "first")
	gitTest(t, "-C", work, "branch", "-M", "main")
	return work, strings.TrimSpace(gitOutput(t, "-C", work, "rev-parse", "HEAD"))
}

func agentStateEvent(t *testing.T, identifier, sha string, createdAt int64) event.Event {
	t.Helper()
	return signedEvent(t, testAgentSecret, event.KIND_REPO_STATE, createdAt, [][]string{{"d", identifier}, {"HEAD", "ref: refs/heads/main"}, {"refs/heads/main", sha}}, "")
}

func waitForRef(t *testing.T, tenant *Tenant, owner, identifier, ref, sha string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := tenant.git.BrowseRepository(owner, identifier); err == nil && r.Refs[ref] == sha {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not reach %s", ref, sha)
}

func TestAgentMaintainGrantMakesTheAgentARepositoryMaintainer(t *testing.T) {
	ctx := context.Background()
	app, tenant := testTenant(t)
	p := tenant.Policy()
	p.Features.Grasp = true
	if err := tenant.applyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	owner, _ := event.PublicKey(testOwnerSecret)
	agent, _ := event.PublicKey(testAgentSecret)
	now := time.Now().Unix()
	work, sha := agentTestRepo(t, tenant, "agentrepo")
	repo := gitrelay.Repository{Owner: owner, Identifier: "agentrepo"}
	coordinate := "30617:" + owner + ":agentrepo"
	grantWith := func(level string, createdAt int64) event.Event {
		return agentGrantEvent(t, agent, createdAt, createdAt+3600, []string{"repo", owner + ":agentrepo:" + level}, []string{"k", "30618"}, []string{"k", "1632"})
	}

	// A read grant lets the agent publish, but it is not a maintainer.
	if err := publishAs(t, tenant, grantWith("read", now)); err != nil {
		t.Fatalf("read grant rejected: %v", err)
	}
	if err := publishAs(t, tenant, agentStateEvent(t, "agentrepo", sha, now+1)); err == nil || !strings.Contains(err.Error(), "not an accepted repository maintainer") {
		t.Fatalf("state under a read grant: %v", err)
	}
	if tenant.IsMaintainer(ctx, repo, agent) {
		t.Fatal("read grant counted as maintainer")
	}

	// A maintain grant makes the agent a maintainer: its state is accepted
	// and a push matching that state goes through.
	if err := publishAs(t, tenant, grantWith("maintain", now+2)); err != nil {
		t.Fatalf("maintain grant rejected: %v", err)
	}
	if !tenant.IsMaintainer(ctx, repo, agent) {
		t.Fatal("maintain grant did not count as maintainer")
	}
	if err := publishAs(t, tenant, agentStateEvent(t, "agentrepo", sha, now+3)); err != nil {
		t.Fatalf("state under a maintain grant rejected: %v", err)
	}
	server := httptest.NewServer(app)
	defer server.Close()
	url := server.URL + "/r/main/" + testOwnerNPub + "/agentrepo.git"
	gitTest(t, "-C", work, "push", url, "refs/heads/main:refs/heads/main")
	waitForRef(t, tenant, owner, "agentrepo", "refs/heads/main", sha)

	// Browse output names the agent as a maintainer with its grant name.
	raw, _ := json.Marshal(map[string]any{"owner": owner, "repo": "agentrepo", "view": "tree"})
	result, err := tenant.Execute(ctx, "", "browserepo", []json.RawMessage{raw})
	if err != nil {
		t.Fatalf("browserepo: %v", err)
	}
	page, ok := result.(gitrelay.BrowsePage)
	if !ok {
		t.Fatalf("browserepo result %T", result)
	}
	if len(page.Maintainers) != 2 || page.Maintainers[0] != (gitrelay.Maintainer{PubKey: owner, Role: "owner"}) || page.Maintainers[1] != (gitrelay.Maintainer{PubKey: agent, Role: "agent", Name: "helper"}) {
		t.Fatalf("maintainers = %+v", page.Maintainers)
	}

	// The agent's status change counts on an issue it did not open.
	issue := signedEvent(t, testOwnerSecret, event.KIND_GIT_ISSUE, now+4, [][]string{{"a", coordinate}, {"p", owner}, {"subject", "Bug"}}, "Details")
	if err := publishAs(t, tenant, issue); err != nil {
		t.Fatalf("issue rejected: %v", err)
	}
	closed := signedEvent(t, testAgentSecret, 1632, now+5, [][]string{{"a", coordinate}, {"e", issue.ID, "", "root"}, {"p", owner}}, "")
	if err := publishAs(t, tenant, closed); err != nil {
		t.Fatalf("agent status rejected: %v", err)
	}
	raw, _ = json.Marshal(map[string]any{"owner": owner, "repo": "agentrepo", "event": issue.ID})
	result, err = tenant.Execute(ctx, agent, "browseissue", []json.RawMessage{raw})
	if err != nil {
		t.Fatalf("browseissue: %v", err)
	}
	detail := result.(collaborationDetail)
	if detail.Item.Status != "closed" || !detail.CanStatus {
		t.Fatalf("status = %q can_status = %v, want closed by the agent maintainer", detail.Item.Status, detail.CanStatus)
	}
	maintainers, _ := detail.Repository["maintainers"].([]gitrelay.Maintainer)
	if len(maintainers) != 2 || maintainers[1].Role != "agent" {
		t.Fatalf("detail maintainers = %+v", maintainers)
	}

	// A second commit needs a new state; a paused grant signs nothing that
	// counts, so the push is refused by the signed-ref hook.
	if err := osWrite(filepath.Join(work, "README"), []byte("agent again\n")); err != nil {
		t.Fatal(err)
	}
	gitTest(t, "-C", work, "commit", "-am", "second")
	next := strings.TrimSpace(gitOutput(t, "-C", work, "rev-parse", "HEAD"))
	if _, err := tenant.community.SetAgentPaused(ctx, owner, agent, true); err != nil {
		t.Fatal(err)
	}
	if tenant.IsMaintainer(ctx, repo, agent) {
		t.Fatal("paused grant counted as maintainer")
	}
	if err := publishAs(t, tenant, agentStateEvent(t, "agentrepo", next, now+6)); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("state under a paused grant: %v", err)
	}
	if out, err := exec.Command("git", "-C", work, "push", url, "refs/heads/main:refs/heads/main").CombinedOutput(); err == nil {
		t.Fatalf("push without a matching signed state succeeded: %s", out)
	}
	if page, _ := tenant.Execute(ctx, "", "browserepo", []json.RawMessage{raw}); len(page.(gitrelay.BrowsePage).Maintainers) != 1 {
		t.Fatalf("paused agent still listed: %+v", page.(gitrelay.BrowsePage).Maintainers)
	}

	// Resumed, the agent maintains again; revoked, it never does.
	if _, err := tenant.community.SetAgentPaused(ctx, owner, agent, false); err != nil {
		t.Fatal(err)
	}
	if !tenant.IsMaintainer(ctx, repo, agent) {
		t.Fatal("resumed grant did not count as maintainer")
	}
	if err := publishAs(t, tenant, agentStateEvent(t, "agentrepo", next, now+7)); err != nil {
		t.Fatalf("state after resume rejected: %v", err)
	}
	gitTest(t, "-C", work, "push", url, "refs/heads/main:refs/heads/main")
	waitForRef(t, tenant, owner, "agentrepo", "refs/heads/main", next)
	if _, err := tenant.community.RevokeAgent(ctx, owner, agent, now+8); err != nil {
		t.Fatal(err)
	}
	if tenant.IsMaintainer(ctx, repo, agent) {
		t.Fatal("revoked grant counted as maintainer")
	}
	if err := publishAs(t, tenant, agentStateEvent(t, "agentrepo", sha, now+9)); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("state under a revoked grant: %v", err)
	}
}

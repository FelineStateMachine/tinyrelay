package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestRepoURLsUseMemberNameWhenPresent(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	named := wikiKey(t, testMemberSecret)
	wikiMember(t, tenant, named)
	base := strings.TrimRight(tenant.publicURL, "/")
	// The owner has no member name, so the hex key stays in the path.
	if got := tenant.repoPageURL(ctx, owner, "notes"); got != base+"/repos/"+owner+"/notes" {
		t.Fatalf("owner repo url %q", got)
	}
	// A named member is written by name, escaped for the path.
	if got := tenant.repoPageURL(ctx, named, "my notes"); got != base+"/repos/member-"+named[:8]+"/my%20notes" {
		t.Fatalf("member repo url %q", got)
	}
	if got := tenant.repoItemURL(ctx, named, "notes", "issues", strings.Repeat("1", 64)); got != base+"/repos/member-"+named[:8]+"/notes/issues/"+strings.Repeat("1", 64) {
		t.Fatalf("member issue url %q", got)
	}
	// A request about a pull request in the member's repository resolves
	// the same way.
	pull := strings.Repeat("2", 64)
	e := event.Event{Kind: kindComment, Tags: [][]string{{"request", "approve"}, {"a", "30617:" + named + ":notes"}, {"E", pull}, {"K", "1618"}}}
	if about := tenant.approvalAboutFrom(ctx, e); about == nil || about.URL != base+"/repos/member-"+named[:8]+"/notes/prs/"+pull {
		t.Fatalf("about %+v", about)
	}
	// A stranger with no membership is written as hex too.
	stranger := strings.Repeat("f", 64)
	if got := tenant.repoPageURL(ctx, stranger, "notes"); got != base+"/repos/"+stranger+"/notes" {
		t.Fatalf("stranger repo url %q", got)
	}
}

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

const collaborationCoordinateRepo = "collaboration"

func collaborationFixtureRepo(t *testing.T, tenant *Tenant) (string, string) {
	t.Helper()
	ctx := context.Background()
	ownerSecret := strings.Repeat("0", 63) + "1"
	owner := tenant.Policy().Owner
	serviceURL, err := url.Parse(tenant.RelayURL())
	if err != nil {
		t.Fatal(err)
	}
	cloneURL := "http://" + serviceURL.Host + "/" + owner + "/" + collaborationCoordinateRepo + ".git"
	relayURL := "ws://" + serviceURL.Host
	announcement := event.Event{
		Kind:      event.KIND_REPO,
		PubKey:    owner,
		CreatedAt: 100,
		Tags:      [][]string{{"d", collaborationCoordinateRepo}, {"clone", cloneURL}, {"relays", relayURL}},
	}
	if err := event.Sign(&announcement, ownerSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.router.Publish(ctx, announcement, relay.Session{PubKeys: []string{owner}}); err != nil {
		t.Fatal(err)
	}
	return owner, "30617:" + owner + ":" + collaborationCoordinateRepo
}

func signedCollaborationEvent(t *testing.T, secret string, kind int, created int64, tags [][]string, content string) event.Event {
	t.Helper()
	e := event.Event{Kind: kind, CreatedAt: created, Tags: tags, Content: content}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	return e
}

func saveCollaborationEvents(t *testing.T, tenant *Tenant, rows ...event.Event) {
	t.Helper()
	for _, row := range rows {
		if _, err := tenant.store.Save(context.Background(), row, storage.SaveOptions{Now: row.CreatedAt, SearchMode: tenant.Policy().Features.Search}); err != nil {
			t.Fatal(err)
		}
	}
}

func browseCollaboration(t *testing.T, tenant *Tenant, actor, method string, args map[string]any) map[string]any {
	t.Helper()
	params, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	value, err := tenant.Execute(context.Background(), actor, method, []json.RawMessage{params})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s result = %T, want object", method, value)
	}
	return result
}

func browseCollaborationDetail(t *testing.T, tenant *Tenant, actor, method string, args map[string]any) collaborationDetail {
	t.Helper()
	params, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	value, err := tenant.Execute(context.Background(), actor, method, []json.RawMessage{params})
	if err != nil {
		t.Fatal(err)
	}
	detail, ok := value.(collaborationDetail)
	if !ok {
		t.Fatalf("%s result = %T, want collaboration detail", method, value)
	}
	return detail
}

func TestCollaborationListPaginationKeepsTimestampTies(t *testing.T) {
	_, tenant := testTenant(t)
	p := tenant.Policy()
	p.Features.Grasp = true
	p.Features.Grasp02 = true
	p.Features.Grasp05 = true
	p.GuestReplies = true
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	owner, coordinate := collaborationFixtureRepo(t, tenant)
	secret := strings.Repeat("1", 63) + "2"
	var ids []string
	for i := 0; i < 9; i++ {
		row := signedCollaborationEvent(t, secret, event.KIND_GIT_ISSUE, 200,
			[][]string{{"a", coordinate}, {"subject", fmt.Sprintf("Issue %02d", i)}}, "body")
		saveCollaborationEvents(t, tenant, row)
		ids = append(ids, row.ID)
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 20; page++ {
		result := browseCollaboration(t, tenant, owner, "browseissues", map[string]any{"owner": owner, "repo": collaborationCoordinateRepo, "limit": 2, "cursor": cursor})
		items, ok := result["items"].([]collaborationItem)
		if !ok {
			t.Fatalf("items = %T", result["items"])
		}
		for _, item := range items {
			id := item.ID
			if seen[id] {
				t.Fatalf("duplicate item %s", id)
			}
			seen[id] = true
		}
		cursor, _ = result["next_cursor"].(string)
		if cursor == "" {
			break
		}
	}
	if len(seen) != len(ids) {
		t.Fatalf("pagination returned %d issues, want %d", len(seen), len(ids))
	}
}

func TestCollaborationSearchAndStatusReachPastFirstPage(t *testing.T) {
	_, tenant := testTenant(t)
	p := tenant.Policy()
	p.Features.Grasp = true
	p.Features.Grasp02 = true
	p.Features.Grasp05 = true
	p.GuestReplies = true
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	owner, coordinate := collaborationFixtureRepo(t, tenant)
	secret := strings.Repeat("2", 63) + "3"
	for i := 0; i < 60; i++ {
		row := signedCollaborationEvent(t, secret, event.KIND_GIT_ISSUE, int64(300+i),
			[][]string{{"a", coordinate}, {"subject", fmt.Sprintf("Routine issue %02d", i)}}, "ordinary body")
		if i == 59 {
			row.Tags = append(row.Tags, []string{"t", "security"})
			row.Content = "Security regression"
		}
		saveCollaborationEvents(t, tenant, row)
		if i == 59 {
			status := signedCollaborationEvent(t, strings.Repeat("0", 63)+"1", 1632, 500,
				[][]string{{"e", row.ID}}, "closed")
			saveCollaborationEvents(t, tenant, status)
		}
	}
	result := browseCollaboration(t, tenant, owner, "browseissues", map[string]any{"owner": owner, "repo": collaborationCoordinateRepo, "limit": 2, "q": "security", "label": "security"})
	items := result["items"].([]collaborationItem)
	if len(items) != 1 || items[0].Status != "closed" {
		t.Fatalf("search result = %#v", result)
	}
	filtered := browseCollaboration(t, tenant, owner, "browseissues", map[string]any{"owner": owner, "repo": collaborationCoordinateRepo, "limit": 2, "state": "closed"})
	if len(filtered["items"].([]collaborationItem)) != 1 {
		t.Fatalf("status filter = %#v", filtered)
	}
}

func TestCollaborationReplyPaginationUsesCursorAndFiltersMalformedEvents(t *testing.T) {
	_, tenant := testTenant(t)
	p := tenant.Policy()
	p.Features.Grasp = true
	p.Features.Grasp02 = true
	p.Features.Grasp05 = true
	p.GuestReplies = true
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	owner, coordinate := collaborationFixtureRepo(t, tenant)
	authorSecret := strings.Repeat("8", 63) + "9"
	author, err := event.PublicKey(authorSecret)
	if err != nil {
		t.Fatal(err)
	}
	issue := signedCollaborationEvent(t, authorSecret, event.KIND_GIT_ISSUE, 800,
		[][]string{{"a", coordinate}, {"subject", "Crowded thread"}}, "body")
	rows := []event.Event{issue}
	for i := 0; i < 205; i++ {
		rows = append(rows, signedCollaborationEvent(t, authorSecret, 1111, 801,
			[][]string{{"E", issue.ID}, {"K", "1621"}, {"P", author}}, fmt.Sprintf("reply %03d", i)))
	}
	rows = append(rows, signedCollaborationEvent(t, authorSecret, 1111, 801,
		[][]string{{"E", issue.ID}, {"K", "1618"}, {"P", author}}, "wrong root kind"))
	rows = append(rows, signedCollaborationEvent(t, authorSecret, 1111, 801,
		[][]string{{"E", issue.ID}, {"K", "1621"}, {"P", strings.Repeat("a", 64)}}, "wrong root author"))
	saveCollaborationEvents(t, tenant, rows...)
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 20; page++ {
		result := browseCollaborationDetail(t, tenant, owner, "browseissue", map[string]any{"owner": owner, "repo": collaborationCoordinateRepo, "event": issue.ID, "limit": 37, "cursor": cursor})
		if len(result.Replies) > 37 {
			t.Fatalf("reply page has %d entries", len(result.Replies))
		}
		for _, reply := range result.Replies {
			if seen[reply.ID] {
				t.Fatalf("duplicate reply %s", reply.ID)
			}
			seen[reply.ID] = true
		}
		cursor = result.ReplyCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 205 {
		t.Fatalf("reply pagination returned %d replies, want 205", len(seen))
	}
}

func TestCollaborationDetailSeparatesRepliesAndAuthorizesStatus(t *testing.T) {
	_, tenant := testTenant(t)
	p := tenant.Policy()
	p.Features.Grasp = true
	p.Features.Grasp02 = true
	p.Features.Grasp05 = true
	p.GuestReplies = true
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	owner, coordinate := collaborationFixtureRepo(t, tenant)
	authorSecret := strings.Repeat("3", 63) + "4"
	author, err := event.PublicKey(authorSecret)
	if err != nil {
		t.Fatal(err)
	}
	issue := signedCollaborationEvent(t, authorSecret, event.KIND_GIT_ISSUE, 600,
		[][]string{{"a", coordinate}, {"subject", "Needs review"}, {"t", "bug"}}, "Please investigate")
	reply := signedCollaborationEvent(t, strings.Repeat("4", 63)+"5", 1111, 601,
		[][]string{{"e", issue.ID}, {"E", issue.ID}, {"K", "1621"}, {"P", author}}, "I can reproduce this")
	status := signedCollaborationEvent(t, authorSecret, 1631, 602, [][]string{{"e", issue.ID}}, "resolved")
	forged := signedCollaborationEvent(t, strings.Repeat("5", 63)+"6", 1632, 603, [][]string{{"e", issue.ID}}, "closed")
	saveCollaborationEvents(t, tenant, issue, reply, status, forged)
	for _, actor := range []string{owner, author} {
		result := browseCollaborationDetail(t, tenant, actor, "browseissue", map[string]any{"owner": owner, "repo": collaborationCoordinateRepo, "event": issue.ID})
		if !result.CanStatus {
			t.Fatalf("actor %s cannot status: %#v", actor, result)
		}
		if len(result.Replies) != 1 || len(result.Statuses) != 1 {
			t.Fatalf("authorized detail thread leaked or duplicated events: %#v", result)
		}
	}
	unauthorized := strings.Repeat("6", 63) + "7"
	unauthorizedKey, err := event.PublicKey(unauthorized)
	if err != nil {
		t.Fatal(err)
	}
	result := browseCollaborationDetail(t, tenant, unauthorizedKey, "browseissue", map[string]any{"owner": owner, "repo": collaborationCoordinateRepo, "event": issue.ID})
	if result.CanStatus {
		t.Fatalf("unauthorized actor can status: %#v", result)
	}
}

func TestPrivateCollaborationRejectsAnonymousAndWrongRepository(t *testing.T) {
	ctx := context.Background()
	secret := strings.Repeat("7", 64)
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	_, tenant, server := privatePeerTenant(t, "collaboration-private", owner)
	repo, _, _ := seedPrivateRepository(t, tenant, owner, secret, "private", server.URL)
	coordinate := "30617:" + owner + ":" + repo.Identifier
	issue := signedCollaborationEvent(t, secret, event.KIND_GIT_ISSUE, 700,
		[][]string{{"a", coordinate}, {"subject", "Private issue"}}, "secret")
	saveCollaborationEvents(t, tenant, issue)
	params := []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"owner":%q,"repo":%q,"event":%q}`, owner, repo.Identifier, issue.ID))}
	if _, err := tenant.Execute(ctx, "", "browseissue", params); err == nil {
		t.Fatal("anonymous private collaboration detail was exposed")
	}
	_, _, _ = seedPrivateRepository(t, tenant, owner, secret, "other", server.URL)
	if _, err := tenant.Execute(ctx, owner, "browseissue", []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"owner":%q,"repo":%q,"event":%q}`, owner, "other", issue.ID))}); err == nil {
		t.Fatal("collaboration root crossed repository boundary")
	}
}

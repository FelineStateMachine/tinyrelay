package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func saveSocialRegression(t *testing.T, tenant *Tenant, rows ...event.Event) {
	t.Helper()
	for _, row := range rows {
		if _, err := tenant.store.Save(context.Background(), row, storage.SaveOptions{Now: row.CreatedAt, SearchMode: tenant.Policy().Features.Search}); err != nil {
			t.Fatal(err)
		}
	}
}

func socialRegressionBrowse(t *testing.T, tenant *Tenant, params map[string]any) map[string]any {
	return socialRegressionBrowseAs(t, tenant, tenant.Policy().Owner, params)
}

func socialRegressionBrowseAs(t *testing.T, tenant *Tenant, actor string, params map[string]any) map[string]any {
	t.Helper()
	value, err := tenant.Execute(context.Background(), actor, "browsesocial", []json.RawMessage{rawJSON(params)})
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)
}

func TestSocialRegressionHiddenActivityAndProfilesAreNotFolded(t *testing.T) {
	_, tenant := testTenant(t)
	secret := strings.Repeat("a", 63) + "b"
	note := signedEvent(t, secret, 1, 100, nil, "visible note")
	comment := signedEvent(t, secret, 1, 101, [][]string{{"e", note.ID, "", "root", note.PubKey}}, "hidden comment")
	reaction := signedEvent(t, secret, 7, 102, [][]string{{"e", note.ID}, {"p", note.PubKey}}, "+")
	profile := signedEvent(t, secret, 0, 103, nil, `{"name":"leaked name","picture":"https://example.test/avatar.png"}`)
	saveSocialRegression(t, tenant, note, comment, reaction, profile)
	visible := socialRegressionBrowse(t, tenant, map[string]any{"limit": 10})["items"].([]socialItem)
	if len(visible) != 1 || visible[0].ReplyCount != 1 || len(visible[0].Reactions) != 1 || visible[0].AuthorName != "leaked name" {
		t.Fatalf("visible activity was not folded before hiding: %#v", visible)
	}
	ctx := context.Background()
	for _, row := range []event.Event{comment, reaction, profile} {
		if _, err := tenant.store.DB().ExecContext(ctx, `INSERT INTO hidden_events(id,reason) VALUES(?,'regression')`, row.ID); err != nil {
			t.Fatal(err)
		}
	}
	result := socialRegressionBrowse(t, tenant, map[string]any{"limit": 10})
	items := result["items"].([]socialItem)
	if len(items) != 1 || items[0].ID != note.ID {
		t.Fatalf("items = %#v", items)
	}
	if items[0].ReplyCount != 0 || len(items[0].Reactions) != 0 {
		t.Fatalf("hidden activity folded into note: %#v", items[0])
	}
	if items[0].AuthorName == "leaked name" || items[0].AuthorPicture != "" {
		t.Fatalf("hidden profile leaked: %#v", items[0])
	}
	tenant.policy.Reads = "members"
	_, err := tenant.Execute(context.Background(), "", "browsesocial", []json.RawMessage{rawJSON(map[string]any{"limit": 10})})
	if err == nil || !strings.Contains(err.Error(), "members-only") {
		t.Fatalf("members-only social feed guest error = %v", err)
	}
}

func TestSocialRegressionSearchCursorCrossesSparsePages(t *testing.T) {
	_, tenant := testTenant(t)
	secret := strings.Repeat("b", 63) + "c"
	newest := signedEvent(t, secret, 1, 1000, nil, "needle newest")
	rows := []event.Event{newest}
	for at := int64(999); at >= 850; at-- {
		rows = append(rows, signedEvent(t, secret, 1, at, nil, "filler"))
	}
	oldest := signedEvent(t, secret, 1, 1, nil, "needle oldest")
	rows = append(rows, oldest)
	saveSocialRegression(t, tenant, rows...)
	first := socialRegressionBrowse(t, tenant, map[string]any{"q": "needle", "limit": 1})
	items := first["items"].([]socialItem)
	if len(items) != 1 || items[0].ID != newest.ID || first["next_cursor"] == "" {
		t.Fatalf("first search page = %#v", first)
	}
	second := socialRegressionBrowse(t, tenant, map[string]any{"q": "needle", "limit": 1, "cursor": first["next_cursor"]})
	items = second["items"].([]socialItem)
	if len(items) != 1 || items[0].ID != oldest.ID {
		t.Fatalf("second search page = %#v", second)
	}
}

func TestSocialRegressionAddressRevisionKeepsCommentsAndReactions(t *testing.T) {
	_, tenant := testTenant(t)
	authorSecret := strings.Repeat("c", 63) + "d"
	commenterSecret := strings.Repeat("e", 63) + "f"
	old := signedEvent(t, authorSecret, 30023, 100, [][]string{{"d", "release"}, {"title", "Old"}}, "old")
	current := signedEvent(t, authorSecret, 30023, 200, [][]string{{"d", "release"}, {"title", "Current"}}, "current")
	address := "30023:" + current.PubKey + ":release"
	comment := signedEvent(t, commenterSecret, 1111, 201, [][]string{{"A", address}, {"K", "30023"}, {"P", current.PubKey}, {"a", address}, {"k", "30023"}, {"p", current.PubKey}}, "comment survives revision")
	reaction := signedEvent(t, commenterSecret, 7, 202, [][]string{{"a", address}, {"e", old.ID}}, "+")
	saveSocialRegression(t, tenant, old, current, comment, reaction)
	result := socialRegressionBrowse(t, tenant, map[string]any{"kind": "articles", "limit": 10})
	items := result["items"].([]socialItem)
	if len(items) != 1 || items[0].Address != address || items[0].Title != "Current" {
		t.Fatalf("canonical article = %#v", items)
	}
	if items[0].ReplyCount != 1 || len(items[0].Reactions) != 1 || items[0].Reactions[0].Count != 1 {
		t.Fatalf("address activity lost: %#v", items[0])
	}
}

func TestSocialRegressionNIP10MentionIsNotReply(t *testing.T) {
	_, tenant := testTenant(t)
	secret := strings.Repeat("d", 63) + "e"
	mentioned := signedEvent(t, secret, 1, 100, nil, "mentioned")
	note := signedEvent(t, secret, 1, 101, [][]string{{"e", mentioned.ID, "", "mention", mentioned.PubKey}, {"q", mentioned.ID}, {"p", mentioned.PubKey}}, "mention only")
	saveSocialRegression(t, tenant, mentioned, note)
	result := socialRegressionBrowse(t, tenant, map[string]any{"limit": 10})
	items := result["items"].([]socialItem)
	if len(items) != 2 {
		t.Fatalf("mention-only note omitted: %#v", items)
	}
	for _, item := range items {
		if item.ID == note.ID && (item.ParentID != "" || item.RootID != note.ID) {
			t.Fatalf("mention-only note treated as reply: %#v", item)
		}
	}
}

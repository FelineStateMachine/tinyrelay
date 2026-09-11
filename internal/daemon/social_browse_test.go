package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestSocialBrowseChronologicalAndThread(t *testing.T) {
	_, tenant := testTenant(t)
	owner := tenant.Policy().Owner
	secret := strings.Repeat("7", 63) + "8"
	note := signedEvent(t, secret, 1, 200, [][]string{{"subject", "A note"}}, "hello markdown")
	article := signedEvent(t, secret, 30023, 100, [][]string{{"d", "essay"}, {"title", "An essay"}, {"summary", "reference"}}, "long form")
	comment := signedEvent(t, secret, 1, 300, [][]string{{"e", note.ID, "", "root", note.PubKey}}, "reply")
	for _, row := range []event.Event{note, article, comment} {
		if _, err := tenant.store.Save(context.Background(), row, storage.SaveOptions{Now: row.CreatedAt, SearchMode: tenant.Policy().Features.Search}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(map[string]any{"limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	value, err := tenant.Execute(context.Background(), owner, "browsesocial", []json.RawMessage{raw})
	if err != nil {
		t.Fatal(err)
	}
	feed := value.(map[string]any)
	items := feed["items"].([]socialItem)
	if len(items) != 2 || items[0].ID != note.ID || items[1].ID != article.ID {
		t.Fatalf("items = %#v", items)
	}
	check, _ := tenant.store.Query(context.Background(), event.Filter{Kinds: []int{1}, Tags: map[string][]string{"e": []string{note.ID}}}, storage.QueryOptions{Now: 999, Access: storage.Access{All: true}})
	if len(check.Events) != 1 {
		t.Fatalf("stored comments = %d", len(check.Events))
	}
	if items[0].ReplyCount != 1 {
		t.Fatalf("reply count = %d", items[0].ReplyCount)
	}
	detail, err := tenant.Execute(context.Background(), owner, "browsesocialthread", []json.RawMessage{json.RawMessage(`{"id":"` + note.ID + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.(map[string]any)["comments"].([]socialItem)) != 1 {
		t.Fatal("comment missing")
	}
}

func TestSocialBrowseDoesNotExposeUnrelatedComments(t *testing.T) {
	_, tenant := testTenant(t)
	owner := tenant.Policy().Owner
	secret := strings.Repeat("8", 63) + "9"
	note := signedEvent(t, secret, 1, 100, nil, "note")
	comment := signedEvent(t, secret, 1111, 200, [][]string{{"e", strings.Repeat("a", 64)}, {"k", "1"}}, "private-looking")
	for _, row := range []event.Event{note, comment} {
		if _, err := tenant.store.Save(context.Background(), row, storage.SaveOptions{Now: row.CreatedAt, SearchMode: tenant.Policy().Features.Search}); err != nil {
			t.Fatal(err)
		}
	}
	value, err := tenant.Execute(context.Background(), owner, "browsesocial", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(value.(map[string]any)["items"].([]socialItem)) != 1 {
		t.Fatal("unrelated comment leaked into feed")
	}
}

func TestSocialBrowseNIP10NIP22AndRevisionReactions(t *testing.T) {
	_, tenant := testTenant(t)
	owner := tenant.Policy().Owner
	authorSecret := strings.Repeat("1", 63) + "2"
	otherSecret := strings.Repeat("3", 63) + "4"
	note := signedEvent(t, authorSecret, 1, 500, nil, "root note")
	legacyReply := signedEvent(t, otherSecret, 1, 501, [][]string{{"e", note.ID, ""}, {"e", note.ID, ""}}, "legacy reply")
	nestedReply := signedEvent(t, authorSecret, 1, 502, [][]string{{"e", note.ID, "", "root", note.PubKey}, {"e", legacyReply.ID, "", "reply", legacyReply.PubKey}}, "nested reply")
	articleOld := signedEvent(t, authorSecret, 30023, 400, [][]string{{"d", "essay:edition"}, {"title", "Old"}}, "old article")
	article := signedEvent(t, authorSecret, 30023, 600, [][]string{{"d", "essay:edition"}, {"title", "Current"}}, "current article")
	articleAddress := "30023:" + article.PubKey + ":essay:edition"
	articleComment := signedEvent(t, otherSecret, 1111, 601, [][]string{
		{"A", articleAddress}, {"E", articleOld.ID, "", articleOld.PubKey}, {"K", "30023"}, {"P", article.PubKey},
		{"a", articleAddress}, {"e", articleOld.ID, "", articleOld.PubKey}, {"k", "30023"}, {"p", articleOld.PubKey},
	}, "article comment")
	reactionOld := signedEvent(t, otherSecret, 7, 602, [][]string{{"e", articleOld.ID}}, "")
	reactionNew := signedEvent(t, otherSecret, 7, 603, [][]string{{"e", article.ID}, {"a", articleAddress}}, "")
	strangerComment := signedEvent(t, otherSecret, 1111, 604, [][]string{{"E", strings.Repeat("f", 64)}, {"K", "30023"}}, "unrelated")
	for _, row := range []event.Event{note, legacyReply, nestedReply, articleOld, article, articleComment, reactionOld, reactionNew, strangerComment} {
		if _, err := tenant.store.Save(context.Background(), row, storage.SaveOptions{Now: row.CreatedAt, SearchMode: tenant.Policy().Features.Search}); err != nil {
			t.Fatal(err)
		}
	}
	if check, queryErr := tenant.store.Query(context.Background(), event.Filter{Kinds: []int{1111}, Tags: map[string][]string{"A": {articleAddress}}}, storage.QueryOptions{Now: 999, Access: storage.Access{All: true}}); queryErr != nil || len(check.Events) != 1 {
		t.Fatalf("article comment query = %d, %v", len(check.Events), queryErr)
	}
	value, err := tenant.Execute(context.Background(), owner, "browsesocial", []json.RawMessage{rawJSON(map[string]any{"limit": 20})})
	if err != nil {
		t.Fatal(err)
	}
	feed := value.(map[string]any)
	items := feed["items"].([]socialItem)
	if len(items) != 2 || items[0].Kind != 30023 || items[1].Kind != 1 {
		t.Fatalf("feed roots = %#v", items)
	}
	articleItem := items[0]
	if articleItem.Title != "Current" || articleItem.Address != articleAddress || articleItem.ReplyCount != 1 {
		t.Fatalf("article item = %#v", articleItem)
	}
	if len(articleItem.Reactions) != 1 || articleItem.Reactions[0].Content != "+" || articleItem.Reactions[0].Count != 1 {
		t.Fatalf("article reactions = %#v", articleItem.Reactions)
	}
	if items[1].ReplyCount != 2 {
		t.Fatalf("note replies = %#v", items[1])
	}
	if items[1].RootID != items[1].ID || items[1].ParentID != "" || items[1].RootKind != 1 {
		t.Fatalf("root metadata = %#v", items[1])
	}
	detail, err := tenant.Execute(context.Background(), owner, "browsesocialthread", []json.RawMessage{rawJSON(map[string]any{"id": nestedReply.ID, "limit": 20})})
	if err != nil {
		t.Fatal(err)
	}
	selected := detail.(map[string]any)["post"].(socialItem)
	if selected.ParentID != legacyReply.ID || selected.RootID != note.ID || selected.ParentKind != 1 {
		t.Fatalf("nested metadata = %#v", selected)
	}
	articleDetail, err := tenant.Execute(context.Background(), owner, "browsesocialthread", []json.RawMessage{rawJSON(map[string]any{"address": articleAddress, "limit": 20})})
	if err != nil {
		t.Fatal(err)
	}
	if len(articleDetail.(map[string]any)["comments"].([]socialItem)) != 1 {
		t.Fatal("article comment missing")
	}
}

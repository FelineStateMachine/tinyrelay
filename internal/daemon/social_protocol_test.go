package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// These fixtures use the wire shapes from NIP-10 and NIP-22 rather than the
// relay's internal metadata vocabulary. They are intentionally signed and
// executed through the tenant RPC so filtering, moderation and visibility are
// exercised together.
func TestSocialProtocolReferencesAndVisibility(t *testing.T) {
	_, tenant := testTenant(t)
	owner := tenant.Policy().Owner
	authorSecret := strings.Repeat("1", 63) + "2"
	commenterSecret := strings.Repeat("3", 63) + "4"
	note := signedEvent(t, authorSecret, 1, 500, nil, "root note")
	reply := signedEvent(t, commenterSecret, 1, 501, [][]string{{"e", note.ID, "", "root", note.PubKey}}, "reply")
	nested := signedEvent(t, authorSecret, 1, 502, [][]string{{"e", note.ID, "", "root", note.PubKey}, {"e", reply.ID, "", "reply", reply.PubKey}}, "nested")
	article := signedEvent(t, authorSecret, 30023, 600, [][]string{{"d", "essay:edition"}, {"title", "Current"}}, "article")
	address := "30023:" + article.PubKey + ":essay:edition"
	articleComment := signedEvent(t, commenterSecret, 1111, 601, [][]string{
		{"A", address}, {"K", "30023"}, {"P", article.PubKey},
		{"a", address}, {"k", "30023"}, {"p", article.PubKey},
	}, "article comment")
	reaction := signedEvent(t, commenterSecret, 7, 602, [][]string{{"e", article.ID}, {"a", address}}, "")
	unrelated := signedEvent(t, commenterSecret, 1111, 603, [][]string{{"E", strings.Repeat("f", 64)}, {"K", "30023"}}, "unrelated")
	for _, row := range []event.Event{note, reply, nested, article, articleComment, reaction, unrelated} {
		if _, err := tenant.store.Save(context.Background(), row, storage.SaveOptions{Now: row.CreatedAt, SearchMode: tenant.Policy().Features.Search}); err != nil {
			t.Fatal(err)
		}
	}
	value, err := tenant.Execute(context.Background(), owner, "browsesocial", []json.RawMessage{rawJSON(map[string]any{"limit": 20})})
	if err != nil {
		t.Fatal(err)
	}
	items := value.(map[string]any)["items"].([]socialItem)
	if len(items) != 2 || items[0].Address != address || items[1].ID != note.ID {
		t.Fatalf("feed roots = %#v", items)
	}
	if items[0].ReplyCount != 1 || len(items[0].Reactions) != 1 || items[0].Reactions[0].Content != "+" {
		t.Fatalf("article folding = %#v", items[0])
	}
	detail, err := tenant.Execute(context.Background(), owner, "browsesocialthread", []json.RawMessage{rawJSON(map[string]any{"id": nested.ID, "limit": 20})})
	if err != nil {
		t.Fatal(err)
	}
	selected := detail.(map[string]any)["post"].(socialItem)
	if selected.ParentID != reply.ID || selected.RootID != note.ID {
		t.Fatalf("thread metadata = %#v", selected)
	}
	rootDetail, err := tenant.Execute(context.Background(), owner, "browsesocialthread", []json.RawMessage{rawJSON(map[string]any{"id": note.ID})})
	if err != nil {
		t.Fatal(err)
	}
	comments := rootDetail.(map[string]any)["comments"].([]socialItem)
	if len(comments) != 2 || comments[0].ParentID != reply.ID {
		t.Fatalf("root conversation = %#v", comments)
	}
	articleDetail, err := tenant.Execute(context.Background(), owner, "browsesocialthread", []json.RawMessage{rawJSON(map[string]any{"address": address, "limit": 20})})
	if err != nil {
		t.Fatal(err)
	}
	if len(articleDetail.(map[string]any)["comments"].([]socialItem)) != 1 {
		t.Fatal("article comment missing")
	}
}

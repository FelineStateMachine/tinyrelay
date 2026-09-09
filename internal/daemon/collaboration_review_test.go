package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
)

func TestReviewAnchorShape(t *testing.T) {
	base := [][]string{{"a", "30617:" + strings.Repeat("a", 64) + ":test"}, {"E", strings.Repeat("b", 64), "", strings.Repeat("a", 64)}, {"K", "1618"}, {"P", strings.Repeat("a", 64)}}
	comment := func(kind string, extra ...[]string) event.Event {
		tags := append([][]string(nil), base...)
		tags[2] = []string{"K", kind}
		return event.Event{Kind: 1111, Tags: append(tags, extra...)}
	}
	anchor, present, err := reviewAnchorOf(comment("1618", []string{"file", "src/main.go"}, []string{"line", "12", "new"}))
	if err != nil || !present || anchor != (reviewAnchor{File: "src/main.go", Line: 12, Side: "new"}) {
		t.Fatalf("valid anchor: %+v %v %v", anchor, present, err)
	}
	if _, present, err := reviewAnchorOf(comment("1621")); err != nil || present {
		t.Fatalf("plain comment: %v %v", present, err)
	}
	cases := map[string][][]string{
		"missing line":    {{"file", "a"}},
		"missing file":    {{"line", "1", "new"}},
		"two files":       {{"file", "a"}, {"file", "b"}, {"line", "1", "new"}},
		"empty path":      {{"file", " "}, {"line", "1", "new"}},
		"zero line":       {{"file", "a"}, {"line", "0", "new"}},
		"padded line":     {{"file", "a"}, {"line", "01", "new"}},
		"word line":       {{"file", "a"}, {"line", "one", "new"}},
		"no side":         {{"file", "a"}, {"line", "1"}},
		"unknown side":    {{"file", "a"}, {"line", "1", "left"}},
		"newline in path": {{"file", "a\nb"}, {"line", "1", "new"}},
	}
	for name, extra := range cases {
		if err := validateReviewAnchor(comment("1618", extra...)); err == nil || !strings.HasPrefix(err.Error(), "blocked: ") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := validateReviewAnchor(comment("1621", []string{"file", "a"}, []string{"line", "1", "new"})); err == nil || !strings.Contains(err.Error(), "pull request or patch") {
		t.Fatalf("anchor under an issue: %v", err)
	}
	if err := validateReviewAnchor(comment("1617", []string{"file", "a"}, []string{"line", "3", "old"})); err != nil {
		t.Fatalf("anchor under a patch: %v", err)
	}
}

func TestReviewCommentsAreValidatedAndBrowsedWithTheirAnchor(t *testing.T) {
	ctx := context.Background()
	_, tenant := testTenant(t)
	p := tenant.Policy()
	p.Features.Grasp = true
	if err := tenant.applyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	owner, _ := event.PublicKey(testOwnerSecret)
	now := time.Now().Unix()
	agentTestRepo(t, tenant, "review")
	coordinate := "30617:" + owner + ":review"
	pull := signedEvent(t, testOwnerSecret, event.KIND_GIT_PR, now, [][]string{{"a", coordinate}, {"p", owner}, {"subject", "Change"}, {"c", strings.Repeat("1", 40)}, {"clone", "https://example.test/review.git"}}, "Why")
	if err := publishAs(t, tenant, pull); err != nil {
		t.Fatalf("pull request rejected: %v", err)
	}
	comment := func(createdAt int64, extra ...[]string) event.Event {
		tags := append([][]string{{"a", coordinate}, {"E", pull.ID, "", owner}, {"K", "1618"}, {"P", owner}, {"e", pull.ID, "", owner}, {"k", "1618"}, {"p", owner}}, extra...)
		return signedEvent(t, testOwnerSecret, 1111, createdAt, tags, "Review note")
	}
	if err := publishAs(t, tenant, comment(now+1, []string{"file", "README"}, []string{"line", "2", "left"})); err == nil || !strings.HasPrefix(err.Error(), "blocked: a review comment") {
		t.Fatalf("malformed anchor (bad side): %v", err)
	}
	if err := publishAs(t, tenant, comment(now+2, []string{"file", "README"}, []string{"line", "2"})); err == nil || !strings.HasPrefix(err.Error(), "blocked: a review comment") {
		t.Fatalf("malformed anchor (no side): %v", err)
	}
	if err := tenant.ingest(ctx, comment(now+3, []string{"file", ""}, []string{"line", "2", "new"}), replication.OriginImport); err == nil || !strings.HasPrefix(err.Error(), "blocked: a review comment") {
		t.Fatalf("malformed anchor over import: %v", err)
	}
	anchored := comment(now+4, []string{"file", "README"}, []string{"line", "2", "new"})
	if err := publishAs(t, tenant, anchored); err != nil {
		t.Fatalf("anchored comment rejected: %v", err)
	}
	plain := comment(now + 5)
	if err := publishAs(t, tenant, plain); err != nil {
		t.Fatalf("plain comment rejected: %v", err)
	}
	raw, _ := json.Marshal(map[string]any{"owner": owner, "repo": "review", "event": pull.ID})
	result, err := tenant.Execute(ctx, "", "browsepull", []json.RawMessage{raw})
	if err != nil {
		t.Fatalf("browsepull: %v", err)
	}
	encoded, _ := json.Marshal(result)
	var detail struct {
		Replies []struct {
			ID   string `json:"id"`
			File string `json:"file"`
			Line int    `json:"line"`
			Side string `json:"side"`
		} `json:"replies"`
	}
	if err := json.Unmarshal(encoded, &detail); err != nil || len(detail.Replies) != 2 {
		t.Fatalf("replies = %s %v", encoded, err)
	}
	if detail.Replies[0].ID != plain.ID || detail.Replies[0].File != "" || detail.Replies[0].Line != 0 || detail.Replies[0].Side != "" {
		t.Fatalf("plain reply carried an anchor: %+v", detail.Replies[0])
	}
	if detail.Replies[1].ID != anchored.ID || detail.Replies[1].File != "README" || detail.Replies[1].Line != 2 || detail.Replies[1].Side != "new" {
		t.Fatalf("anchored reply = %+v", detail.Replies[1])
	}
}

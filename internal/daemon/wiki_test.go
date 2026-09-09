package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
)

const (
	wikiOwnerSecret  = "0000000000000000000000000000000000000000000000000000000000000001"
	wikiMemberSecret = "2222222222222222222222222222222222222222222222222222222222222222"
	wikiOtherSecret  = "3333333333333333333333333333333333333333333333333333333333333333"
	wikiThirdSecret  = "4444444444444444444444444444444444444444444444444444444444444444"
)

func wikiKey(t *testing.T, secret string) string {
	t.Helper()
	pk, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	return pk
}

func wikiPublish(t *testing.T, tenant *Tenant, secret string, kind int, createdAt int64, content string, tags ...[]string) event.Event {
	t.Helper()
	e := event.Event{Kind: kind, CreatedAt: createdAt, Content: content, Tags: tags}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.Publish(context.Background(), e, relay.Session{PubKeys: []string{e.PubKey}, RelayURL: tenant.RelayURL()}); err != nil {
		t.Fatalf("publish kind %d: %v", kind, err)
	}
	return e
}

func wikiArticle(t *testing.T, tenant *Tenant, secret, title string, createdAt int64, extra ...[]string) event.Event {
	t.Helper()
	tags := append([][]string{{"d", strings.ToLower(strings.ReplaceAll(title, " ", "-"))}, {"title", title}, {"summary", "Summary about " + title}}, extra...)
	return wikiPublish(t, tenant, secret, kindWikiArticle, createdAt, "Content of "+title+" with a [[Wiki Link]].", tags...)
}

func wikiMember(t *testing.T, tenant *Tenant, pubkey string) {
	t.Helper()
	if _, err := tenant.community.Execute(context.Background(), tenant.Policy().Owner, "setmember", []json.RawMessage{json.RawMessage(fmt.Sprintf("%q", pubkey)), json.RawMessage(fmt.Sprintf(`{"name":"member-%s"}`, pubkey[:8]))}); err != nil {
		t.Fatal(err)
	}
}

func wikiCall(t *testing.T, tenant *Tenant, actor, method string, params map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	value, err := tenant.Execute(context.Background(), actor, method, []json.RawMessage{raw})
	if err != nil {
		t.Fatalf("%s %s: %v", method, raw, err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func wikiItems(result map[string]any, key string) []map[string]any {
	var items []map[string]any
	for _, item := range result[key].([]any) {
		items = append(items, item.(map[string]any))
	}
	return items
}

func TestWikiPreferenceOrder(t *testing.T) {
	_, tenant := testTenant(t)
	owner := tenant.Policy().Owner
	member, other, third := wikiKey(t, wikiMemberSecret), wikiKey(t, wikiOtherSecret), wikiKey(t, wikiThirdSecret)
	wikiMember(t, tenant, member)
	wikiMember(t, tenant, third)
	now := time.Now().Unix()

	ownerBitcoin := wikiArticle(t, tenant, wikiOwnerSecret, "Bitcoin", now-300)
	memberBitcoin := wikiArticle(t, tenant, wikiMemberSecret, "Bitcoin", now-200)
	otherBitcoin := wikiArticle(t, tenant, wikiOtherSecret, "Bitcoin", now-100)

	memberLightning := wikiArticle(t, tenant, wikiMemberSecret, "Lightning", now-300)
	otherLightning := wikiArticle(t, tenant, wikiOtherSecret, "Lightning", now-200)
	wikiPublish(t, tenant, wikiOwnerSecret, kindReaction, now-150, "+", []string{"e", memberLightning.ID})
	wikiPublish(t, tenant, wikiThirdSecret, kindReaction, now-140, "", []string{"a", "30818:" + member + ":lightning"})
	wikiPublish(t, tenant, wikiThirdSecret, kindReaction, now-130, "-", []string{"e", memberLightning.ID})
	// Non-members do not count, and one member counts once per version.
	wikiPublish(t, tenant, wikiOtherSecret, kindReaction, now-120, "+", []string{"e", otherLightning.ID})
	wikiPublish(t, tenant, wikiThirdSecret, kindReaction, now-110, "+", []string{"e", memberLightning.ID})

	wikiArticle(t, tenant, wikiMemberSecret, "Nostr", now-300)
	otherNostr := wikiArticle(t, tenant, wikiOtherSecret, "Nostr", now-200)

	page := wikiCall(t, tenant, "", "browsewikipage", map[string]any{"d": "bitcoin"})
	version := page["version"].(map[string]any)
	if version["author"] != owner || page["preferred_by"] != "owner" || version["id"] != ownerBitcoin.ID {
		t.Fatalf("owner preference %v", page)
	}
	if version["content"] == "" || fmt.Sprint(version["links"]) != "[wiki-link]" {
		t.Fatalf("preferred version content %v", version)
	}
	versions := wikiItems(page, "versions")
	if len(versions) != 3 || versions[0]["id"] != otherBitcoin.ID || versions[1]["id"] != memberBitcoin.ID || versions[2]["id"] != ownerBitcoin.ID {
		t.Fatalf("versions newest first %v", versions)
	}
	if _, ok := versions[0]["content"]; ok {
		t.Fatalf("version list carries content %v", versions[0])
	}
	page = wikiCall(t, tenant, "", "browsewikipage", map[string]any{"d": "Bitcoin", "author": member})
	if page["version"].(map[string]any)["id"] != memberBitcoin.ID || page["preferred_by"] != "author" {
		t.Fatalf("author preference %v", page)
	}
	page = wikiCall(t, tenant, "", "browsewikipage", map[string]any{"d": "bitcoin", "version": otherBitcoin.ID})
	if page["version"].(map[string]any)["id"] != otherBitcoin.ID || page["preferred_by"] != "version" {
		t.Fatalf("version selection %v", page)
	}
	if _, err := tenant.Execute(context.Background(), "", "browsewikipage", []json.RawMessage{json.RawMessage(`{"d":"bitcoin","version":"` + strings.Repeat("f", 64) + `"}`)}); err == nil {
		t.Fatal("unknown version selected")
	}

	page = wikiCall(t, tenant, "", "browsewikipage", map[string]any{"d": "lightning"})
	version = page["version"].(map[string]any)
	if version["id"] != memberLightning.ID || page["preferred_by"] != "reactions" || version["likes"].(float64) != 2 {
		t.Fatalf("reaction preference %v", page)
	}
	for _, v := range wikiItems(page, "versions") {
		if v["id"] == otherLightning.ID && v["likes"].(float64) != 0 {
			t.Fatalf("non-member like counted %v", v)
		}
	}

	page = wikiCall(t, tenant, "", "browsewikipage", map[string]any{"d": "nostr"})
	if page["version"].(map[string]any)["id"] != otherNostr.ID || page["preferred_by"] != "newest" {
		t.Fatalf("newest preference %v", page)
	}

	list := wikiCall(t, tenant, "", "browsewiki", nil)
	items := wikiItems(list, "items")
	if len(items) != 3 || items[0]["d"] != "bitcoin" || items[1]["d"] != "lightning" || items[2]["d"] != "nostr" || list["next_cursor"] != "" {
		t.Fatalf("page list %v", list)
	}
	if items[0]["versions"].(float64) != 3 || items[0]["version"].(map[string]any)["author"] != owner || items[1]["version"].(map[string]any)["id"] != memberLightning.ID {
		t.Fatalf("page list preference %v", items)
	}
	list = wikiCall(t, tenant, "", "browsewiki", map[string]any{"author": other})
	if wikiItems(list, "items")[0]["version"].(map[string]any)["author"] != other {
		t.Fatalf("author preference in list %v", list)
	}
}

func TestWikiSearchAndPagination(t *testing.T) {
	_, tenant := testTenant(t)
	now := time.Now().Unix()
	names := []string{"Alpha", "Beta", "Delta", "Epsilon", "Gamma"}
	for i, name := range names {
		wikiArticle(t, tenant, wikiOwnerSecret, name, now-int64(i))
	}
	var got []string
	cursor := ""
	for page := 0; page < 5; page++ {
		result := wikiCall(t, tenant, "", "browsewiki", map[string]any{"limit": 2, "cursor": cursor})
		for _, item := range wikiItems(result, "items") {
			got = append(got, item["d"].(string))
		}
		cursor = result["next_cursor"].(string)
		if cursor == "" {
			break
		}
	}
	if fmt.Sprint(got) != "[alpha beta delta epsilon gamma]" {
		t.Fatalf("paginated pages = %v", got)
	}
	result := wikiCall(t, tenant, "", "browsewiki", map[string]any{"q": "about BETA"})
	if items := wikiItems(result, "items"); len(items) != 1 || items[0]["d"] != "beta" {
		t.Fatalf("summary search %v", result)
	}
	result = wikiCall(t, tenant, "", "browsewiki", map[string]any{"q": "gamm"})
	if items := wikiItems(result, "items"); len(items) != 1 || items[0]["title"] != "Gamma" {
		t.Fatalf("title search %v", result)
	}
	if items := wikiItems(wikiCall(t, tenant, "", "browsewiki", map[string]any{"q": "zeta"}), "items"); len(items) != 0 {
		t.Fatalf("unmatched search %v", items)
	}
}

func TestWikiMergeRequestStatusFollowsDestinationReactions(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	member := wikiKey(t, wikiMemberSecret)
	wikiMember(t, tenant, member)
	now := time.Now().Unix()
	original := wikiArticle(t, tenant, wikiOwnerSecret, "Bitcoin", now-500)
	coordinate := "30818:" + owner + ":bitcoin"
	fork := wikiArticle(t, tenant, wikiMemberSecret, "Bitcoin", now-400, []string{"a", coordinate, "", "fork"}, []string{"e", original.ID, "", "fork"})
	merge := wikiPublish(t, tenant, wikiMemberSecret, kindWikiMerge, now-300, "Added the block size limit", []string{"a", coordinate}, []string{"e", original.ID}, []string{"p", owner}, []string{"e", fork.ID, "", "source"})

	detail := wikiCall(t, tenant, "", "browsewikimerge", map[string]any{"id": merge.ID})
	m := detail["merge"].(map[string]any)
	if m["status"] != "open" || m["source"] != fork.ID || m["base"] != original.ID || m["destination"] != owner || m["target_d"] != "bitcoin" {
		t.Fatalf("merge detail %v", m)
	}
	if detail["proposed"].(map[string]any)["id"] != fork.ID || detail["target"].(map[string]any)["id"] != original.ID {
		t.Fatalf("merge versions %v", detail)
	}
	if proposed := detail["proposed"].(map[string]any); proposed["fork"].(map[string]any)["e"] != original.ID || proposed["fork"].(map[string]any)["a"] != coordinate {
		t.Fatalf("fork reference %v", proposed)
	}
	page := wikiCall(t, tenant, "", "browsewikipage", map[string]any{"d": "bitcoin"})
	if merges := wikiItems(page, "merges"); len(merges) != 1 || merges[0]["id"] != merge.ID || merges[0]["status"] != "open" {
		t.Fatalf("page merges %v", page)
	}
	if items := wikiItems(wikiCall(t, tenant, "", "browsewiki", nil), "items"); items[0]["open_merges"].(float64) != 1 {
		t.Fatalf("open merge count %v", items)
	}

	// Only the destination author's reaction answers the request.
	wikiPublish(t, tenant, wikiMemberSecret, kindReaction, now-250, "+", []string{"e", merge.ID}, []string{"p", owner})
	if answer, err := tenant.wikiMergeStatus(ctx, merge.ID, owner); err != nil || answer.Status != wikiMergeOpen {
		t.Fatalf("foreign reaction %+v %v", answer, err)
	}
	accept := wikiPublish(t, tenant, wikiOwnerSecret, kindReaction, now-200, "+", []string{"e", merge.ID}, []string{"p", member})
	answer, err := tenant.wikiMergeStatus(ctx, merge.ID, owner)
	if err != nil || answer.Status != wikiMergeAccepted || answer.AnsweredAt != now-200 || answer.Reaction != accept.ID {
		t.Fatalf("accepted %+v %v", answer, err)
	}
	reject := wikiPublish(t, tenant, wikiOwnerSecret, kindReaction, now-100, "-", []string{"e", merge.ID})
	answer, err = tenant.wikiMergeStatus(ctx, merge.ID, owner)
	if err != nil || answer.Status != wikiMergeRejected || answer.Reaction != reject.ID {
		t.Fatalf("rejected %+v %v", answer, err)
	}
	// Other reaction content does not answer.
	wikiPublish(t, tenant, wikiOwnerSecret, kindReaction, now-50, "🤔", []string{"e", merge.ID})
	if answer, _ := tenant.wikiMergeStatus(ctx, merge.ID, owner); answer.Status != wikiMergeRejected {
		t.Fatalf("emoji reaction changed the answer %+v", answer)
	}
	detail = wikiCall(t, tenant, "", "browsewikimerge", map[string]any{"event": merge.ID})
	if detail["merge"].(map[string]any)["status"] != "rejected" {
		t.Fatalf("merge detail status %v", detail)
	}
	if items := wikiItems(wikiCall(t, tenant, "", "browsewiki", nil), "items"); items[0]["open_merges"].(float64) != 0 {
		t.Fatalf("answered merge still open %v", items)
	}
	if _, err := tenant.Execute(ctx, "", "browsewikimerge", []json.RawMessage{json.RawMessage(`{"id":"` + original.ID + `"}`)}); err == nil {
		t.Fatal("article served as a merge request")
	}
}

func TestWikiRedirects(t *testing.T) {
	_, tenant := testTenant(t)
	owner := tenant.Policy().Owner
	now := time.Now().Unix()
	wikiArticle(t, tenant, wikiOwnerSecret, "Bitcoin", now-100)
	redirect := wikiPublish(t, tenant, wikiMemberSecret, kindWikiRedirect, now-50, "", []string{"d", "btc"}, []string{"a", "30818:" + owner + ":bitcoin"})
	page := wikiCall(t, tenant, "", "browsewikipage", map[string]any{"d": "bitcoin"})
	if to := wikiItems(page, "redirects_to"); len(to) != 1 || to[0]["d"] != "btc" || to[0]["id"] != redirect.ID || len(wikiItems(page, "redirects_from")) != 0 {
		t.Fatalf("redirects to %v", page)
	}
	page = wikiCall(t, tenant, "", "browsewikipage", map[string]any{"d": "BTC"})
	if from := wikiItems(page, "redirects_from"); page["version"] != nil || len(from) != 1 || from[0]["target_d"] != "bitcoin" || from[0]["target_author"] != owner {
		t.Fatalf("redirects from %v", page)
	}
	if _, err := tenant.Execute(context.Background(), "", "browsewikipage", []json.RawMessage{json.RawMessage(`{"d":"missing"}`)}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing page %v", err)
	}
}

func TestWikiMembersOnlyHidesPagesFromGuests(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	member, other := wikiKey(t, wikiMemberSecret), wikiKey(t, wikiOtherSecret)
	wikiMember(t, tenant, member)
	wikiArticle(t, tenant, wikiOwnerSecret, "Bitcoin", time.Now().Unix()-10)
	if items := wikiItems(wikiCall(t, tenant, "", "browsewiki", nil), "items"); len(items) != 1 {
		t.Fatalf("open relay list %v", items)
	}
	p := tenant.Policy()
	p.Reads = "members"
	if err := tenant.applyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"", other} {
		for _, method := range []string{"browsewiki", "browsewikipage", "browsewikimerge"} {
			if _, err := tenant.Execute(ctx, actor, method, []json.RawMessage{json.RawMessage(`{"d":"bitcoin","id":"` + strings.Repeat("a", 64) + `"}`)}); err == nil {
				t.Fatalf("%s exposed to %q", method, actor)
			}
		}
	}
	if items := wikiItems(wikiCall(t, tenant, member, "browsewiki", nil), "items"); len(items) != 1 {
		t.Fatalf("member list %v", items)
	}
	if page := wikiCall(t, tenant, tenant.Policy().Owner, "browsewikipage", map[string]any{"d": "bitcoin"}); page["version"] == nil {
		t.Fatalf("owner page %v", page)
	}
}

func TestWikiMergeRequestWakesDestinationDevices(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	member, other := wikiKey(t, wikiMemberSecret), wikiKey(t, wikiOtherSecret)
	wikiMember(t, tenant, member)
	now := time.Now().Unix()
	for _, pubkey := range []string{owner, other} {
		if _, err := tenant.store.DB().ExecContext(ctx, "INSERT INTO web_push(endpoint,pubkey,subscription,created_at) VALUES(?,?,?,?)", "https://push.example/"+pubkey[:8], pubkey, "{}", now); err != nil {
			t.Fatal(err)
		}
	}
	wikiArticle(t, tenant, wikiOwnerSecret, "Bitcoin Basics", now-100)
	// A request aimed at someone who is not a member, and one the owner
	// sends to themselves, wake nobody.
	wikiPublish(t, tenant, wikiMemberSecret, kindWikiMerge, now-90, "", []string{"a", "30818:" + other + ":bitcoin-basics"}, []string{"p", other}, []string{"e", strings.Repeat("a", 64), "", "source"})
	wikiPublish(t, tenant, wikiOwnerSecret, kindWikiMerge, now-80, "", []string{"a", "30818:" + owner + ":bitcoin-basics"}, []string{"p", owner}, []string{"e", strings.Repeat("b", 64), "", "source"})
	if got := countIntents(t, tenant, notificationPush); got != 0 {
		t.Fatalf("unexpected notifications %d", got)
	}
	merge := wikiPublish(t, tenant, wikiMemberSecret, kindWikiMerge, now-70, "Please merge", []string{"a", "30818:" + owner + ":bitcoin-basics"}, []string{"p", owner}, []string{"e", strings.Repeat("c", 64), "", "source"})
	var payload string
	if err := tenant.store.DB().QueryRowContext(ctx, "SELECT payload FROM work_intents WHERE kind=? AND target=?", notificationPush, owner).Scan(&payload); err != nil {
		t.Fatalf("notification intent: %v", err)
	}
	var notice pushPayload
	if err := json.Unmarshal([]byte(payload), &notice); err != nil {
		t.Fatal(err)
	}
	if notice.Kind != pushReplies || notice.Text != "Merge request for Bitcoin Basics" || notice.URL != "http://relay.test/wiki/bitcoin-basics?merge="+merge.ID {
		t.Fatalf("notice %+v", notice)
	}
}

func TestWikiMergeFromReadsEveryTagShape(t *testing.T) {
	owner := strings.Repeat("a", 64)
	base, source := strings.Repeat("b", 64), strings.Repeat("c", 64)
	coordinate := "30818:" + owner + ":bitcoin"
	cases := []struct {
		name         string
		tags         [][]string
		base, source string
	}{
		{"source marker", [][]string{{"a", coordinate}, {"e", base}, {"p", owner}, {"e", source, "", "source"}}, base, source},
		{"source tag", [][]string{{"a", coordinate}, {"e", base}, {"source", source}}, base, source},
		{"single e", [][]string{{"a", coordinate}, {"e", source}}, "", source},
		{"two plain e", [][]string{{"a", coordinate}, {"e", base}, {"e", source}}, base, source},
	}
	for _, tc := range cases {
		m := wikiMergeFrom(event.Event{Kind: kindWikiMerge, Tags: tc.tags})
		if m.Base != tc.base || m.Source != tc.source || m.Destination != owner || m.TargetD != "bitcoin" || m.Status != wikiMergeOpen {
			t.Errorf("%s: %+v", tc.name, m)
		}
	}
	if m := wikiMergeFrom(event.Event{Kind: kindWikiMerge, Tags: [][]string{{"a", "30617:" + owner + ":repo"}}}); m.Destination != "" || m.TargetD != "" {
		t.Errorf("foreign coordinate accepted %+v", m)
	}
}

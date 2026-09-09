package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/work"
)

// loopbackTenant is a tenant whose public URL is a loopback address, which
// lets a callback point at a local test server.
func loopbackTenant(t *testing.T) (*App, *Tenant) {
	t.Helper()
	ctx := context.Background()
	a, err := New(ctx, Config{DataDir: t.TempDir(), DefaultTenant: "main"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	owner, _ := event.PublicKey(testOwnerSecret)
	meta, err := a.Create(ctx, CreateOptions{Name: "main", Owner: owner, Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := a.tenant(ctx, meta, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	// Fence the production worker so the tests drive each delivery intent
	// themselves.
	tenant.workCancel()
	tenant.workWG.Wait()
	return a, tenant
}

func setRole(t *testing.T, tenant *Tenant, pubkey, role string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"role": role})
	if _, err := tenant.Execute(context.Background(), tenant.Policy().Owner, "setmember", []json.RawMessage{json.RawMessage(strconv.Quote(pubkey)), raw}); err != nil {
		t.Fatal(err)
	}
}

func callbackParams(url string, filter string, extra ...string) []json.RawMessage {
	options := map[string]any{"url": url, "filter": json.RawMessage(filter)}
	if len(extra) > 0 {
		options["secret"] = extra[0]
	}
	raw, _ := json.Marshal(options)
	return []json.RawMessage{raw}
}

func addCallback(t *testing.T, tenant *Tenant, actor, url, filter string) map[string]any {
	t.Helper()
	result, err := tenant.Execute(context.Background(), actor, "addcallback", callbackParams(url, filter))
	if err != nil {
		t.Fatalf("addcallback: %v", err)
	}
	return result.(map[string]any)
}

func TestCallbackRegistrationValidatesURLFilterScopeAndCap(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	member, _ := event.PublicKey(testMemberSecret)
	agent, _ := event.PublicKey(testAgentSecret)
	setRole(t, tenant, member, "member")
	now := time.Now().Unix()
	if err := publishAs(t, tenant, agentGrantEvent(t, agent, now, now+3600, []string{"k", "1"}, []string{"room", "build"}, []string{"repo", owner + ":tinyrelay:read"})); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 9)
	for i := range ids {
		ids[i] = strings.Repeat(strconv.Itoa(i), 64)
	}
	nineIDs, _ := json.Marshal(ids)
	for _, tc := range []struct {
		name, actor, url, filter, want string
	}{
		{"guest", strings.Repeat("e", 64), "https://hooks.example/wake", `{"kinds":[1]}`, "restricted:"},
		{"http url", member, "http://hooks.example/wake", `{"kinds":[1]}`, "invalid: url must be https"},
		{"credentials", member, "https://user:pw@hooks.example/wake", `{"kinds":[1]}`, "invalid: url must be https"},
		{"private address", member, "https://10.0.0.1/wake", `{"kinds":[1]}`, "invalid: url must not name a private"},
		{"loopback address", member, "https://127.0.0.1/wake", `{"kinds":[1]}`, "invalid: url must not name a private"},
		{"localhost", member, "https://localhost/wake", `{"kinds":[1]}`, "invalid: url must name a public host"},
		{"bare host", member, "https://relay/wake", `{"kinds":[1]}`, "invalid: url must name a public host"},
		{"no kinds", member, "https://hooks.example/wake", `{"#p":["` + member + `"]}`, "invalid: filter must name at least one kind"},
		{"unsupported key", member, "https://hooks.example/wake", `{"kinds":[1],"ids":["` + strings.Repeat("a", 64) + `"]}`, "invalid: filter key \"ids\""},
		{"unsupported tag", member, "https://hooks.example/wake", `{"kinds":[1],"#t":["x"]}`, "invalid: filter key \"#t\""},
		{"too many values", member, "https://hooks.example/wake", `{"kinds":[1],"#e":` + string(nineIDs) + `}`, "invalid: #e must list 1 to 8 values"},
		{"bad pubkey", member, "https://hooks.example/wake", `{"kinds":[1],"#p":["nope"]}`, "invalid: #p values"},
		{"bad coordinate", member, "https://hooks.example/wake", `{"kinds":[1621],"#a":["tinyrelay"]}`, "invalid: #a values"},
		{"bad room", member, "https://hooks.example/wake", `{"kinds":[9],"#h":["Not A Room"]}`, "invalid: #h values"},
		{"not an object", member, "https://hooks.example/wake", `[1]`, "invalid: filter must be a JSON object"},
		{"agent room outside grant", agent, "https://hooks.example/wake", `{"kinds":[9],"#h":["general"]}`, "restricted: agent grant does not cover room general"},
		{"agent repo outside grant", agent, "https://hooks.example/wake", `{"kinds":[1621],"#a":["30617:` + owner + `:other"]}`, "restricted: agent grant does not cover repository other"},
	} {
		_, err := tenant.Execute(ctx, tc.actor, "addcallback", callbackParams(tc.url, tc.filter))
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want prefix %q", tc.name, err, tc.want)
		}
	}
	if _, err := tenant.Execute(ctx, member, "addcallback", callbackParams("https://hooks.example/wake", `{"kinds":[1]}`, "short")); err == nil || !strings.HasPrefix(err.Error(), "invalid: secret must be") {
		t.Fatalf("short secret: %v", err)
	}
	// An agent may watch the room and repository its grant covers, and any
	// kind, including ones it may not publish.
	inside := addCallback(t, tenant, agent, "https://hooks.example/wake", `{"kinds":[7,9,1621],"#h":["build"],"#a":["30617:`+owner+`:tinyrelay"],"since":1}`)
	if inside["owner"] != agent || inside["host"] != "hooks.example" || inside["paused"] != false {
		t.Fatalf("agent callback = %v", inside)
	}
	secret, _ := inside["secret"].(string)
	if len(secret) != 64 {
		t.Fatalf("generated secret = %q", secret)
	}
	filter, _ := json.Marshal(inside["filter"])
	if string(filter) != `{"#a":["30617:`+owner+`:tinyrelay"],"#h":["build"],"kinds":[7,9,1621]}` {
		t.Fatalf("stored filter = %s", filter)
	}
	// A member gets four callbacks; the owner has no cap.
	for i := 0; i < 4; i++ {
		addCallback(t, tenant, member, "https://hooks.example/wake/"+strconv.Itoa(i), `{"kinds":[1]}`)
	}
	if _, err := tenant.Execute(ctx, member, "addcallback", callbackParams("https://hooks.example/wake/5", `{"kinds":[1]}`)); err == nil || err.Error() != "invalid: at most 4 callbacks per key" {
		t.Fatalf("fifth callback: %v", err)
	}
	for i := 0; i < 6; i++ {
		addCallback(t, tenant, owner, "https://hooks.example/owner/"+strconv.Itoa(i), `{"kinds":[1]}`)
	}
	// The secret shows once: listings and the table row never repeat it.
	listed, err := tenant.Execute(ctx, agent, "listcallbacks", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := listed.([]map[string]any)
	if len(rows) != 1 || rows[0]["id"] != inside["id"] || rows[0]["url"] != "https://hooks.example/wake" {
		t.Fatalf("agent listing = %v", rows)
	}
	if _, present := rows[0]["secret"]; present {
		t.Fatal("listing repeats the secret")
	}
	encoded, _ := json.Marshal(rows[0])
	if strings.Contains(string(encoded), secret) {
		t.Fatal("listing carries the secret")
	}
	// Setting the policy allowance to zero closes registration for
	// everyone but the owner.
	if _, err := tenant.Execute(ctx, owner, "setpolicy", []json.RawMessage{json.RawMessage(`{"callbacks":0}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.Execute(ctx, agent, "addcallback", callbackParams("https://hooks.example/again", `{"kinds":[1]}`)); err == nil || err.Error() != "invalid: at most 0 callbacks per key" {
		t.Fatalf("closed registration: %v", err)
	}
	if got := tenant.Policy().Callbacks; got != 0 {
		t.Fatalf("policy callbacks = %d", got)
	}
}

func TestCallbackManagementPermissionMatrix(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	member, _ := event.PublicKey(testMemberSecret)
	moderator, _ := event.PublicKey(testModSecret)
	agent, _ := event.PublicKey(testAgentSecret)
	guest := strings.Repeat("e", 64)
	setRole(t, tenant, member, "member")
	setRole(t, tenant, moderator, "moderator")
	now := time.Now().Unix()
	if err := publishAs(t, tenant, agentGrantEvent(t, agent, now, now+3600, []string{"k", "1"})); err != nil {
		t.Fatal(err)
	}
	methods, err := tenant.Execute(ctx, owner, "supportedmethods", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"listcallbacks", "addcallback", "removecallback", "pausecallback", "resumecallback"} {
		if !containsString(methods.([]string), name) {
			t.Fatalf("supportedmethods lacks %s", name)
		}
	}
	mine := addCallback(t, tenant, member, "https://hooks.example/member?token=abc", `{"kinds":[1]}`)
	theirs := addCallback(t, tenant, agent, "https://hooks.example/agent", `{"kinds":[1]}`)
	id := func(values map[string]any) json.RawMessage {
		return json.RawMessage(strconv.Quote(values["id"].(string)))
	}
	for _, method := range []string{"listcallbacks", "addcallback", "removecallback", "pausecallback", "resumecallback"} {
		if _, err := tenant.Execute(ctx, guest, method, []json.RawMessage{id(mine)}); err == nil || !strings.HasPrefix(err.Error(), "restricted:") {
			t.Fatalf("%s by guest: %v", method, err)
		}
	}
	// A member sees and controls only its own callbacks.
	listed, _ := tenant.Execute(ctx, member, "listcallbacks", nil)
	if rows := listed.([]map[string]any); len(rows) != 1 || rows[0]["id"] != mine["id"] {
		t.Fatalf("member listing = %v", rows)
	}
	for _, method := range []string{"pausecallback", "resumecallback", "removecallback"} {
		if _, err := tenant.Execute(ctx, member, method, []json.RawMessage{id(theirs)}); err == nil || !strings.HasPrefix(err.Error(), "restricted:") {
			t.Fatalf("%s on another key's callback: %v", method, err)
		}
	}
	paused, err := tenant.Execute(ctx, member, "pausecallback", []json.RawMessage{json.RawMessage(`{"id":"` + mine["id"].(string) + `"}`)})
	if err != nil || paused.(map[string]any)["paused"] != true {
		t.Fatalf("pause own: %v %v", paused, err)
	}
	if resumed, err := tenant.Execute(ctx, member, "resumecallback", []json.RawMessage{id(mine)}); err != nil || resumed.(map[string]any)["paused"] != false {
		t.Fatalf("resume own: %v %v", resumed, err)
	}
	// Moderators and the owner see everything, with the host but not the
	// path of other keys' URLs, and may control any callback.
	for _, actor := range []string{moderator, owner} {
		listed, err := tenant.Execute(ctx, actor, "listcallbacks", nil)
		if err != nil {
			t.Fatalf("listcallbacks by %s: %v", actor, err)
		}
		rows := listed.([]map[string]any)
		byOwner := map[string]map[string]any{}
		for _, row := range rows {
			byOwner[row["owner"].(string)] = row
		}
		if len(rows) != 2 || byOwner[member]["host"] != "hooks.example" || byOwner[agent]["id"] != theirs["id"] {
			t.Fatalf("operator listing = %v", rows)
		}
		if _, present := byOwner[member]["url"]; present {
			t.Fatalf("operator listing carries another key's URL path: %v", byOwner[member])
		}
		if _, err := tenant.Execute(ctx, actor, "pausecallback", []json.RawMessage{id(theirs)}); err != nil {
			t.Fatalf("pause by %s: %v", actor, err)
		}
		if _, err := tenant.Execute(ctx, actor, "resumecallback", []json.RawMessage{id(theirs)}); err != nil {
			t.Fatalf("resume by %s: %v", actor, err)
		}
	}
	if _, err := tenant.Execute(ctx, owner, "pausecallback", []json.RawMessage{json.RawMessage(`"missing"`)}); err == nil || !strings.HasPrefix(err.Error(), "not found:") {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := tenant.Execute(ctx, owner, "pausecallback", nil); err == nil || !strings.HasPrefix(err.Error(), "invalid:") {
		t.Fatalf("missing id: %v", err)
	}
	removed, err := tenant.Execute(ctx, moderator, "removecallback", []json.RawMessage{id(theirs)})
	if err != nil || removed.(map[string]any)["removed"] != true {
		t.Fatalf("remove by moderator: %v %v", removed, err)
	}
	if _, err := tenant.Execute(ctx, agent, "removecallback", []json.RawMessage{id(mine)}); err == nil || !strings.HasPrefix(err.Error(), "restricted:") {
		t.Fatalf("agent removing a member's callback: %v", err)
	}
	if _, err := tenant.Execute(ctx, member, "removecallback", []json.RawMessage{id(mine)}); err != nil {
		t.Fatalf("remove own: %v", err)
	}
	audit, err := tenant.Execute(ctx, owner, "listaudit", nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range audit.([]community.AuditRow) {
		seen[row.Action] = true
		if strings.Contains(row.Detail, "hooks.example") || strings.Contains(row.Target, "hooks.example") {
			t.Fatalf("audit carries a callback URL: %+v", row)
		}
	}
	for _, action := range []string{"addcallback", "pausecallback", "resumecallback", "removecallback"} {
		if !seen[action] {
			t.Fatalf("audit lacks %s: %v", action, seen)
		}
	}
}

func TestCallbackMatchesFilterAndKeepsPrivateEventsBehindTheGate(t *testing.T) {
	_, tenant := loopbackTenant(t)
	ctx := context.Background()
	member, _ := event.PublicKey(testMemberSecret)
	moderator, _ := event.PublicKey(testModSecret)
	setRole(t, tenant, member, "member")
	setRole(t, tenant, moderator, "moderator")
	now := time.Now().Unix()
	notes := addCallback(t, tenant, member, "https://127.0.0.1:1/notes", `{"kinds":[1],"#p":["`+member+`"]}`)
	messages := addCallback(t, tenant, member, "https://127.0.0.1:1/messages", `{"kinds":[4]}`)
	intents := func() map[string]int {
		rows, err := tenant.store.DB().QueryContext(ctx, "SELECT target,count(*) FROM work_intents WHERE kind=? GROUP BY target", callbackDelivery)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		counts := map[string]int{}
		for rows.Next() {
			var target string
			var count int
			if err := rows.Scan(&target, &count); err != nil {
				t.Fatal(err)
			}
			counts[target] = count
		}
		return counts
	}
	// Kind and tag both have to match.
	if err := publishAs(t, tenant, signedEvent(t, testOwnerSecret, 1, now, [][]string{{"p", moderator}}, "not for the member")); err != nil {
		t.Fatal(err)
	}
	if err := publishAs(t, tenant, signedEvent(t, testOwnerSecret, 7, now, [][]string{{"p", member}}, "+")); err != nil {
		t.Fatal(err)
	}
	if counts := intents(); len(counts) != 0 {
		t.Fatalf("intents after unmatched events = %v", counts)
	}
	if err := publishAs(t, tenant, signedEvent(t, testOwnerSecret, 1, now+1, [][]string{{"p", member}}, "for the member")); err != nil {
		t.Fatal(err)
	}
	if counts := intents(); counts[notes["id"].(string)] != 1 {
		t.Fatalf("intents after matched note = %v", counts)
	}
	// A private message the key may not read is matched by the filter but
	// stopped by the gate; one addressed to the key goes through.
	if err := publishAs(t, tenant, signedEvent(t, testOwnerSecret, 4, now+2, [][]string{{"p", moderator}}, "secret")); err != nil {
		t.Fatal(err)
	}
	if counts := intents(); counts[messages["id"].(string)] != 0 {
		t.Fatalf("private message leaked to the callback: %v", counts)
	}
	if err := publishAs(t, tenant, signedEvent(t, testOwnerSecret, 4, now+3, [][]string{{"p", member}}, "for you")); err != nil {
		t.Fatal(err)
	}
	if counts := intents(); counts[messages["id"].(string)] != 1 {
		t.Fatalf("addressed message not queued: %v", counts)
	}
	// A paused callback leaves the index at once.
	if _, err := tenant.Execute(ctx, member, "pausecallback", []json.RawMessage{json.RawMessage(strconv.Quote(notes["id"].(string)))}); err != nil {
		t.Fatal(err)
	}
	if err := publishAs(t, tenant, signedEvent(t, testOwnerSecret, 1, now+4, [][]string{{"p", member}}, "while paused")); err != nil {
		t.Fatal(err)
	}
	if counts := intents(); counts[notes["id"].(string)] != 1 {
		t.Fatalf("paused callback still queued: %v", counts)
	}
	if got := tenant.Policy().Callbacks; got != 4 {
		t.Fatalf("default callbacks allowance = %d", got)
	}
}

type callbackReceiver struct {
	mu      sync.Mutex
	status  int
	headers []http.Header
	bodies  [][]byte
}

func (r *callbackReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.headers = append(r.headers, req.Header.Clone())
	r.bodies = append(r.bodies, body)
	status := r.status
	r.mu.Unlock()
	w.WriteHeader(status)
}

func (r *callbackReceiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

// pendingCallbackIntents lists queued deliveries of one event to one
// callback, first attempt first.
func pendingCallbackIntents(t *testing.T, tenant *Tenant, id, eventID string) []work.Intent {
	t.Helper()
	rows, err := tenant.store.DB().QueryContext(context.Background(), "SELECT id,event_id,payload,next_at FROM work_intents WHERE kind=? AND target=? AND state='pending' AND (event_id=? OR event_id LIKE ?) ORDER BY next_at,created_at,id", callbackDelivery, id, eventID, eventID+"#%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []work.Intent
	for rows.Next() {
		var item work.Intent
		var next int64
		if err := rows.Scan(&item.ID, &item.EventID, &item.Payload, &next); err != nil {
			t.Fatal(err)
		}
		item.Kind, item.Target, item.NextAt = callbackDelivery, id, time.Unix(next, 0)
		out = append(out, item)
	}
	return out
}

func TestCallbackDeliverySignsBodyRetriesAndPauses(t *testing.T) {
	_, tenant := loopbackTenant(t)
	ctx := context.Background()
	member, _ := event.PublicKey(testMemberSecret)
	setRole(t, tenant, member, "member")
	receiver := &callbackReceiver{status: http.StatusAccepted}
	service := httptest.NewTLSServer(receiver)
	defer service.Close()
	tenant.callbackClient = service.Client()
	registered := addCallback(t, tenant, member, service.URL+"/wake?token=t0k3n", `{"kinds":[1621]}`)
	id, secret := registered["id"].(string), registered["secret"].(string)
	now := time.Now().Unix()
	issue := signedEvent(t, testOwnerSecret, 1621, now, [][]string{{"a", "30617:" + tenant.Policy().Owner + ":tinyrelay"}, {"subject", "Broken build"}}, "The build fails.")
	if err := publishAs(t, tenant, issue); err != nil {
		t.Fatal(err)
	}
	queued := pendingCallbackIntents(t, tenant, id, issue.ID)
	if len(queued) != 1 || queued[0].EventID != issue.ID {
		t.Fatalf("queued = %+v", queued)
	}
	if err := tenant.handleCallbackDelivery(ctx, queued[0]); err != nil {
		t.Fatal(err)
	}
	if receiver.count() != 1 {
		t.Fatalf("deliveries = %d", receiver.count())
	}
	headers, body := receiver.headers[0], receiver.bodies[0]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if headers.Get("Content-Type") != "application/json" || headers.Get("X-Tiny-Callback") != id || headers.Get("X-Tiny-Relay") != "http://127.0.0.1:8080" || headers.Get("X-Tiny-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("headers = %v", headers)
	}
	delivered, err := event.Parse(body)
	if err != nil || delivered.ID != issue.ID || delivered.Content != issue.Content {
		t.Fatalf("body = %s (%v)", body, err)
	}
	var lastAt int64
	var lastStatus string
	var failures, paused int
	state := func() {
		t.Helper()
		if err := tenant.store.DB().QueryRowContext(ctx, "SELECT last_delivery_at,last_status,failures,paused FROM callbacks WHERE id=?", id).Scan(&lastAt, &lastStatus, &failures, &paused); err != nil {
			t.Fatal(err)
		}
	}
	state()
	if lastAt < now || lastStatus != "ok" || failures != 0 || paused != 0 {
		t.Fatalf("after delivery: at=%d status=%q failures=%d paused=%d", lastAt, lastStatus, failures, paused)
	}
	listed, _ := tenant.Execute(ctx, member, "listcallbacks", nil)
	if rows := listed.([]map[string]any); rows[0]["lastStatus"] != "ok" || rows[0]["lastDelivery"].(int64) < now {
		t.Fatalf("listing after delivery = %v", rows)
	}

	// A failing receiver: attempt one queues attempt two a minute out,
	// attempt two queues attempt three five minutes out, and attempt three
	// is the last.
	receiver.status = http.StatusInternalServerError
	second := signedEvent(t, testOwnerSecret, 1621, now+1, [][]string{{"a", "30617:" + tenant.Policy().Owner + ":tinyrelay"}, {"subject", "Again"}}, "Again.")
	if err := publishAs(t, tenant, second); err != nil {
		t.Fatal(err)
	}
	queued = pendingCallbackIntents(t, tenant, id, second.ID)
	if len(queued) != 1 {
		t.Fatalf("queued = %+v", queued)
	}
	if err := tenant.handleCallbackDelivery(ctx, queued[0]); err != nil {
		t.Fatalf("attempt 1 should not fail the intent: %v", err)
	}
	state()
	if failures != 1 || lastStatus != "HTTP 500" || paused != 0 {
		t.Fatalf("after attempt 1: status=%q failures=%d paused=%d", lastStatus, failures, paused)
	}
	retries := pendingCallbackIntents(t, tenant, id, second.ID)
	if len(retries) != 2 || retries[1].EventID != second.ID+"#2" || retries[1].NextAt.Unix() < now+55 || retries[1].NextAt.Unix() > now+70 {
		t.Fatalf("retry after attempt 1 = %+v", retries)
	}
	var payload callbackPayload
	if err := json.Unmarshal([]byte(retries[1].Payload), &payload); err != nil || payload.Attempt != 2 || payload.Event.ID != second.ID {
		t.Fatalf("retry payload = %s", retries[1].Payload)
	}
	if err := tenant.handleCallbackDelivery(ctx, retries[1]); err != nil {
		t.Fatal(err)
	}
	retries = pendingCallbackIntents(t, tenant, id, second.ID)
	if len(retries) != 3 || retries[2].EventID != second.ID+"#3" || retries[2].NextAt.Unix() < now+295 || retries[2].NextAt.Unix() > now+310 {
		t.Fatalf("retry after attempt 2 = %+v", retries)
	}
	if err := tenant.handleCallbackDelivery(ctx, retries[2]); err != nil {
		t.Fatal(err)
	}
	if again := pendingCallbackIntents(t, tenant, id, second.ID); len(again) != 3 {
		t.Fatalf("attempt 3 queued another attempt: %+v", again)
	}
	state()
	if failures != 3 || receiver.count() != 4 {
		t.Fatalf("after three attempts: failures=%d deliveries=%d", failures, receiver.count())
	}
	// Twenty failures in a row pause the callback with the reason, after
	// which nothing more is queued or sent until it is resumed.
	for failures < callbackPauseFailures {
		if err := tenant.handleCallbackDelivery(ctx, retries[2]); err != nil {
			t.Fatal(err)
		}
		state()
	}
	if paused != 1 || !strings.HasPrefix(lastStatus, "paused after 20 failures: HTTP 500") {
		t.Fatalf("after 20 failures: status=%q paused=%d", lastStatus, paused)
	}
	sent := receiver.count()
	third := signedEvent(t, testOwnerSecret, 1621, now+2, [][]string{{"a", "30617:" + tenant.Policy().Owner + ":tinyrelay"}, {"subject", "Third"}}, "Third.")
	if err := publishAs(t, tenant, third); err != nil {
		t.Fatal(err)
	}
	if queued := pendingCallbackIntents(t, tenant, id, third.ID); len(queued) != 0 {
		t.Fatalf("paused callback queued a delivery: %+v", queued)
	}
	if err := tenant.handleCallbackDelivery(ctx, retries[2]); err != nil || receiver.count() != sent {
		t.Fatalf("paused callback delivered: %v %d", err, receiver.count())
	}
	receiver.status = http.StatusOK
	if _, err := tenant.Execute(ctx, member, "resumecallback", []json.RawMessage{json.RawMessage(strconv.Quote(id))}); err != nil {
		t.Fatal(err)
	}
	state()
	if paused != 0 || failures != 0 {
		t.Fatalf("after resume: failures=%d paused=%d", failures, paused)
	}
	fourth := signedEvent(t, testOwnerSecret, 1621, now+3, [][]string{{"a", "30617:" + tenant.Policy().Owner + ":tinyrelay"}, {"subject", "Fourth"}}, "Fourth.")
	if err := publishAs(t, tenant, fourth); err != nil {
		t.Fatal(err)
	}
	queued = pendingCallbackIntents(t, tenant, id, fourth.ID)
	if len(queued) != 1 {
		t.Fatalf("queued after resume = %+v", queued)
	}
	if err := tenant.handleCallbackDelivery(ctx, queued[0]); err != nil || receiver.count() != sent+1 {
		t.Fatalf("delivery after resume: %v %d", err, receiver.count())
	}
	// A removed callback consumes its intents without a POST.
	if _, err := tenant.Execute(ctx, member, "removecallback", []json.RawMessage{json.RawMessage(strconv.Quote(id))}); err != nil {
		t.Fatal(err)
	}
	if err := tenant.handleCallbackDelivery(ctx, queued[0]); err != nil || receiver.count() != sent+1 {
		t.Fatalf("removed callback delivered: %v %d", err, receiver.count())
	}
}

func TestCallbackDeliveryRechecksTheGateAndMembership(t *testing.T) {
	_, tenant := loopbackTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	member, _ := event.PublicKey(testMemberSecret)
	setRole(t, tenant, member, "member")
	receiver := &callbackReceiver{status: http.StatusOK}
	service := httptest.NewTLSServer(receiver)
	defer service.Close()
	tenant.callbackClient = service.Client()
	registered := addCallback(t, tenant, member, service.URL+"/wake", `{"kinds":[1]}`)
	id := registered["id"].(string)
	now := time.Now().Unix()
	note := signedEvent(t, testOwnerSecret, 1, now, nil, "hidden later")
	if err := publishAs(t, tenant, note); err != nil {
		t.Fatal(err)
	}
	queued := pendingCallbackIntents(t, tenant, id, note.ID)
	if len(queued) != 1 {
		t.Fatalf("queued = %+v", queued)
	}
	// Hidden between arrival and delivery: nothing is sent.
	if _, err := tenant.Execute(ctx, owner, "banevent", []json.RawMessage{json.RawMessage(strconv.Quote(note.ID))}); err != nil {
		t.Fatal(err)
	}
	if err := tenant.handleCallbackDelivery(ctx, queued[0]); err != nil || receiver.count() != 0 {
		t.Fatalf("hidden event delivered: %v %d", err, receiver.count())
	}
	// A key that lost its membership has its callback paused.
	if _, err := tenant.Execute(ctx, owner, "removemember", []json.RawMessage{json.RawMessage(strconv.Quote(member))}); err != nil {
		t.Fatal(err)
	}
	if err := tenant.handleCallbackDelivery(ctx, queued[0]); err != nil || receiver.count() != 0 {
		t.Fatalf("former member delivered: %v %d", err, receiver.count())
	}
	var paused int
	var status string
	if err := tenant.store.DB().QueryRowContext(ctx, "SELECT paused,last_status FROM callbacks WHERE id=?", id).Scan(&paused, &status); err != nil || paused != 1 || status != "paused: the key is no longer a member" {
		t.Fatalf("former member callback: paused=%d status=%q %v", paused, status, err)
	}
}

func TestMCPCallbackTools(t *testing.T) {
	app, tenant := testTenant(t)
	member, _ := event.PublicKey(testMemberSecret)
	setRole(t, tenant, member, "member")
	call := func(name string, arguments map[string]any, secret string) (map[string]any, bool) {
		t.Helper()
		w, response := mcpCall{method: "tools/call", name: name, arguments: arguments, sign: true, secret: secret}.do(t, app, "/mcp")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		return mcpToolResult(t, response)
	}
	structured := func(result map[string]any) map[string]any {
		value, _ := result["structuredContent"].(map[string]any)
		inner, _ := value["result"].(map[string]any)
		if inner == nil {
			return value
		}
		return inner
	}
	text := func(result map[string]any) string {
		content, _ := result["content"].([]any)
		if len(content) == 0 {
			return ""
		}
		return content[0].(map[string]any)["text"].(string)
	}
	_, response := mcpCall{method: "tools/list", sign: true}.do(t, app, "/mcp")
	names := map[string]bool{}
	for _, tool := range response.Result.(map[string]any)["tools"].([]any) {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"list_callbacks", "add_callback", "remove_callback", "pause_callback", "resume_callback"} {
		if !names[want] {
			t.Fatalf("missing tool %s", want)
		}
	}
	filter := map[string]any{"kinds": []any{1621.0, 1111.0}, "#p": []any{member}}
	result, isError := call("add_callback", map[string]any{"url": "https://hooks.example/wake", "filter": filter}, strings.Repeat("7", 64))
	if !isError || !strings.Contains(text(result), "restricted") {
		t.Fatalf("add_callback as guest: %v %s", isError, text(result))
	}
	result, isError = call("add_callback", map[string]any{"url": "http://hooks.example/wake", "filter": filter}, testMemberSecret)
	if !isError || !strings.Contains(text(result), "https") {
		t.Fatalf("add_callback with http: %v %s", isError, text(result))
	}
	result, isError = call("add_callback", map[string]any{"url": "https://hooks.example/wake", "filter": filter, "secret": "a-shared-secret-of-my-own"}, testMemberSecret)
	added := structured(result)
	if isError || added["secret"] != "a-shared-secret-of-my-own" || added["host"] != "hooks.example" || added["owner"] != member {
		t.Fatalf("add_callback: %s", text(result))
	}
	id, _ := added["id"].(string)
	result, isError = call("list_callbacks", map[string]any{}, testMemberSecret)
	listed, _ := result["structuredContent"].(map[string]any)["result"].([]any)
	if isError || len(listed) != 1 || listed[0].(map[string]any)["id"] != id || listed[0].(map[string]any)["paused"] != false {
		t.Fatalf("list_callbacks: %s", text(result))
	}
	if strings.Contains(text(result), "a-shared-secret-of-my-own") {
		t.Fatal("list_callbacks repeats the secret")
	}
	result, isError = call("pause_callback", map[string]any{"id": id}, "")
	if isError || structured(result)["paused"] != true {
		t.Fatalf("pause_callback as owner: %s", text(result))
	}
	result, isError = call("resume_callback", map[string]any{"id": id}, testMemberSecret)
	if isError || structured(result)["paused"] != false {
		t.Fatalf("resume_callback: %s", text(result))
	}
	result, isError = call("remove_callback", map[string]any{"id": id}, strings.Repeat("7", 64))
	if !isError {
		t.Fatalf("remove_callback as guest: %s", text(result))
	}
	result, isError = call("remove_callback", map[string]any{"id": id}, testMemberSecret)
	if isError || structured(result)["removed"] != true {
		t.Fatalf("remove_callback: %s", text(result))
	}
	result, _ = call("list_callbacks", map[string]any{}, "")
	if listed, _ := result["structuredContent"].(map[string]any)["result"].([]any); len(listed) != 0 {
		t.Fatalf("list_callbacks after remove: %s", text(result))
	}
	r := httptest.NewRequest(http.MethodGet, "http://relay.test/llms.txt", nil)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "list_callbacks") || !strings.Contains(w.Body.String(), "add_callback") {
		t.Fatalf("llms.txt lacks the callback tools: %s", w.Body.String())
	}
}

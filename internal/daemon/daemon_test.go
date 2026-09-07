package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestTenantIsolationAndDurableHTTPPublish(t *testing.T) {
	ctx := context.Background()
	app, err := New(ctx, Config{DataDir: t.TempDir(), DefaultTenant: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	owner, err := event.PublicKey(strings.Repeat("0", 63) + "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "bob"} {
		if _, err := app.Create(ctx, CreateOptions{Name: name, Owner: owner, Template: "default"}); err != nil {
			t.Fatal(err)
		}
	}
	e := event.Event{Kind: 1, CreatedAt: time.Now().Unix(), Tags: [][]string{}, Content: "hello tiny"}
	if err := event.Sign(&e, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://relay.test/r/alice/events", strings.NewReader(string(raw)))
	signRequest(t, request, string(raw))
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("publish %d %s", response.Code, response.Body.String())
	}
	for _, tc := range []struct {
		name  string
		count int
	}{{"alice", 1}, {"bob", 0}} {
		request := httptest.NewRequest(http.MethodPost, "http://relay.test/r/"+tc.name+"/query", strings.NewReader(`[{"kinds":[1]}]`))
		signRequest(t, request, `[{"kinds":[1]}]`)
		response := httptest.NewRecorder()
		app.ServeHTTP(response, request)
		var events []event.Event
		if err := json.Unmarshal(response.Body.Bytes(), &events); err != nil {
			t.Fatalf("query %s: %s %v", tc.name, response.Body.String(), err)
		}
		if len(events) != tc.count {
			t.Fatalf("%s: got%d want%d", tc.name, len(events), tc.count)
		}
	}
}

func signRequest(t *testing.T, r *http.Request, body string) {
	t.Helper()
	hash := sha256.Sum256([]byte(body))
	e := event.Event{Kind: 27235, CreatedAt: time.Now().Unix(), Content: "", Tags: [][]string{{"u", r.URL.String()}, {"method", r.Method}, {"payload", hex.EncodeToString(hash[:])}}}
	if err := event.Sign(&e, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Nostr "+base64.StdEncoding.EncodeToString(raw))
}

func TestCreationRequiresOwnerAndInformationHasNoEconomy(t *testing.T) {
	ctx := context.Background()
	app, err := New(ctx, Config{DataDir: t.TempDir(), DefaultTenant: "main"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := app.Create(ctx, CreateOptions{Name: "unowned", Template: "default"}); err == nil {
		t.Fatal("unowned relay allowed")
	}
	owner, err := event.PublicKey(strings.Repeat("0", 63) + "1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Create(ctx, CreateOptions{Name: "main", Owner: owner, Template: "default"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://relay.test/", nil)
	req.Header.Set("Accept", "application/nostr+json")
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)
	if res.Code != 200 {
		t.Fatalf("info %d %s", res.Code, res.Body.String())
	}
	var info map[string]json.RawMessage
	if err := json.Unmarshal(res.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"fees", "payments_url", "fuel", "lease"} {
		if _, ok := info[key]; ok {
			t.Fatalf("hosted economy leaked: %s", key)
		}
	}
}

package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestNamedViewsRespectTheirOwnAudience(t *testing.T) {
	app, tenant := testTenant(t)
	ctx := context.Background()
	p := tenant.Policy()
	p.Reads = "open"
	p.DirectoryPublic = false
	if err := tenant.applyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		signed bool
		status int
	}{
		{"articles", false, http.StatusOK},
		{"profiles", false, http.StatusUnauthorized},
		{"profiles", true, http.StatusOK},
		{"missing", false, http.StatusNotFound},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://relay.test/r/main/view/"+tc.name, nil)
		if tc.signed {
			signRequest(t, req, "")
		}
		res := httptest.NewRecorder()
		app.ServeHTTP(res, req)
		if res.Code != tc.status {
			t.Fatalf("%s signed=%v: status %d, %s", tc.name, tc.signed, res.Code, res.Body.String())
		}
		if res.Code == http.StatusOK {
			e, err := event.Parse(res.Body.Bytes())
			if err != nil || event.Tag(e, "d") != "bind.ws/view/"+tc.name {
				t.Fatalf("signed view response: %s, %v", res.Body.String(), err)
			}
			if res.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatal("view response permits shared caching")
			}
		}
	}
	// A members-only projection must not become readable as a stored event.
	var private int
	err := tenant.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE kind=? AND d=?`, event.KIND_VIEW, "bind.ws/view/profiles").Scan(&private)
	if err != nil || private != 0 {
		t.Fatalf("private view persisted: %d, %v", private, err)
	}
}

func TestSchedulerWakesForDurableWriteViewDeadline(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	var first int64
	waitFor := func(check func() bool) {
		t.Helper()
		deadline := time.NewTimer(8 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for !check() {
			select {
			case <-deadline.C:
				t.Fatal("view deadline was not serviced")
			case <-ticker.C:
			}
		}
	}
	waitFor(func() bool {
		return tenant.store.GetSetting(ctx, "records.view.articles.at", &first) == nil && first != 0
	})
	e := event.Event{Kind: 30023, CreatedAt: time.Now().Unix(), Tags: [][]string{{"d", "scheduled"}, {"title", "Scheduled article"}}, Content: "body"}
	if err := event.Sign(&e, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.Save(ctx, e, storage.SaveOptions{Now: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	// Model a durable dirty marker recovered nine seconds into its debounce.
	if err := tenant.records.MarkView(ctx, "articles", time.Now().Unix()-9); err != nil {
		t.Fatal(err)
	}
	tenant.scheduleViews()
	waitFor(func() bool {
		result, err := tenant.store.Query(ctx, event.Filter{Kinds: []int{event.KIND_VIEW}, Tags: map[string][]string{"d": {"bind.ws/view/articles"}}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}})
		if err != nil || len(result.Events) != 1 {
			return false
		}
		return event.Tag(result.Events[0], "a") == "30023:"+e.PubKey+":scheduled"
	})
}

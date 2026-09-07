package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestMountedHTTPPortableImportReportsFinalCountAndContent(t *testing.T) {
	app, tenant := testTenant(t)
	secret := strings.Repeat("0", 63) + "1"
	items := make([]event.Event, 2)
	var body bytes.Buffer
	for i := range items {
		items[i] = event.Event{Kind: 1, CreatedAt: time.Now().Unix() + int64(i), Content: "portable-http-" + string(rune('a'+i))}
		if err := event.Sign(&items[i], secret); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(items[i])
		if err != nil {
			t.Fatal(err)
		}
		body.Write(raw)
		body.WriteByte('\n')
	}
	res := mountedRequest(t, app, "PUT", "/import", body.Bytes(), "", true)
	if res.Code != 200 {
		t.Fatalf("mounted import status=%d body=%s", res.Code, res.Body.String())
	}
	var response struct {
		Result struct {
			Imported int `json:"imported"`
		} `json:"result"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Result.Imported != len(items) {
		t.Fatalf("mounted import count=%d want=%d", response.Result.Imported, len(items))
	}
	for _, item := range items {
		rows, err := tenant.store.Query(context.Background(), event.Filter{IDs: []string{item.ID}, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}})
		if err != nil || len(rows.Events) != 1 || rows.Events[0].Content != item.Content {
			t.Fatalf("imported content missing for %s: %d %v", item.ID, len(rows.Events), err)
		}
	}
}

func TestDaemonPortableImportJobResumesAfterRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	secret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{DataDir: dir, DefaultTenant: "main"}
	app, err := New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := app.Create(ctx, CreateOptions{Name: "main", Owner: owner, Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(ctx, "http://relay.test"); err != nil {
		t.Fatal(err)
	}
	tenant := app.tenants[meta.ID]
	tenant.workCancel()
	tenant.workWG.Wait()
	item := event.Event{Kind: 1, CreatedAt: time.Now().Unix(), Content: "portable import"}
	if err := event.Sign(&item, secret); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(meta.Paths.Root, "resume.jsonl"), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := tenant.replication.AddJob(ctx, replication.JobSpec{ID: "resume-import", Kind: replication.JobImport, Relays: []string{"resume.jsonl"}}); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(ctx); err != nil {
		t.Fatal(err)
	}

	app, err = New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	if err := app.Start(ctx, "http://relay.test"); err != nil {
		t.Fatal(err)
	}
	reloaded := app.tenants[meta.ID]
	if !waitFor(t, 8*time.Second, func() bool {
		statuses, statusErr := reloaded.replication.ListJobStatus(ctx)
		return statusErr == nil && len(statuses) == 1 && statuses[0].Phase == "complete"
	}) {
		statuses, _ := reloaded.replication.ListJobStatus(ctx)
		t.Fatalf("portable import did not resume: %#v", statuses)
	}
	result, err := reloaded.store.Query(ctx, event.Filter{IDs: []string{item.ID}, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}})
	if err != nil || len(result.Events) != 1 {
		t.Fatalf("imported event missing after restart: %d %v", len(result.Events), err)
	}
}

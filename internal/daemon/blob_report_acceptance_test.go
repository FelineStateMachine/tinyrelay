package daemon

import (
	"bytes"
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

	"github.com/FelineStateMachine/tinyrelay/internal/blob"
	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestMountedBlobReportAppearsInCommunityModeration(t *testing.T) {
	ctx := context.Background()
	app, tenant := testTenant(t)
	reporterSecret := strings.Repeat("2", 64)
	reporter, err := event.PublicKey(reporterSecret)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := tenant.blobs.Put(ctx, blobPutOptions("moderated blob", reporter))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	report := event.Event{Kind: event.KIND_REPORT, CreatedAt: now, Tags: [][]string{{"x", entry.SHA256, "spam"}}, Content: "reported blob"}
	if err := event.Sign(&report, reporterSecret); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(report)
	req := httptest.NewRequest(http.MethodPut, "http://relay.test/r/main/report", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	signBlobReport(t, req, body, reporterSecret)
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("report status=%d body=%s", res.Code, res.Body.String())
	}

	value, err := tenant.community.Execute(ctx, tenant.Policy().Owner, "listreports", reportArgs("open"))
	if err != nil {
		t.Fatal(err)
	}
	reports := value.([]community.Report)
	if len(reports) != 1 || reports[0].TargetEvent != entry.SHA256 || reports[0].Type != "blob" {
		t.Fatalf("shared reports=%+v", reports)
	}
	if _, err := tenant.community.Execute(ctx, tenant.Policy().Owner, "resolvereport", reportArgs(reports[0].ID, "dismiss")); err != nil {
		t.Fatal(err)
	}
	var hidden int
	if err := tenant.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM hidden_events WHERE id=?`, entry.SHA256).Scan(&hidden); err != nil {
		t.Fatal(err)
	}
	if hidden != 0 {
		t.Fatal("blob report hid an event")
	}
	var status string
	if err := tenant.store.DB().QueryRowContext(ctx, `SELECT status FROM blob_reports WHERE target_blob=?`, entry.SHA256).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "resolved" {
		t.Fatalf("blob report status=%q", status)
	}
}

func blobPutOptions(content, uploader string) blob.PutOptions {
	return blob.PutOptions{Reader: strings.NewReader(content), Type: "text/plain", Uploader: uploader}
}

func reportArgs(values ...any) []json.RawMessage {
	out := make([]json.RawMessage, len(values))
	for i, value := range values {
		out[i], _ = json.Marshal(value)
	}
	return out
}

func signBlobReport(t *testing.T, req *http.Request, body []byte, secret string) {
	t.Helper()
	hash := sha256.Sum256(body)
	token := event.Event{Kind: 27235, CreatedAt: time.Now().Unix(), Tags: [][]string{{"u", req.URL.String()}, {"method", req.Method}, {"payload", hex.EncodeToString(hash[:])}}}
	if err := event.Sign(&token, secret); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(token)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Nostr "+base64.StdEncoding.EncodeToString(raw))
}

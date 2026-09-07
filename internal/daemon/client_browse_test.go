package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/blob"
)

func TestClientFileBrowsingPermissionsAndDownload(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	entry, err := tenant.blobs.Put(ctx, blob.PutOptions{Reader: strings.NewReader("<script>secret source</script>"), Type: "text/html", Uploader: tenant.Policy().Owner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.Execute(ctx, "", "browsefiles", nil); err == nil {
		t.Fatal("anonymous uploader inventory exposed")
	}
	value, err := tenant.Execute(ctx, tenant.Policy().Owner, "browsefiles", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	if !strings.Contains(string(raw), entry.SHA256) {
		t.Fatalf("missing blob %s", raw)
	}
	req := httptest.NewRequest(http.MethodGet, "http://relay.test/files/raw?hash="+entry.SHA256, nil)
	res := httptest.NewRecorder()
	tenant.ServeHTTP(res, req)
	if res.Code != 200 || res.Body.String() != "<script>secret source</script>" {
		t.Fatalf("download %d %s", res.Code, res.Body.String())
	}
	if !strings.HasPrefix(res.Header().Get("Content-Disposition"), "attachment") || res.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Fatal("active download lacks isolation")
	}
	p := tenant.Policy()
	p.Reads = "members"
	if err := tenant.applyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	res = httptest.NewRecorder()
	tenant.ServeHTTP(res, req)
	if res.Code == 200 {
		t.Fatal("private download exposed")
	}
}

func TestClientStatusRequiresOwnerAndPreservesErrors(t *testing.T) {
	_, tenant := testTenant(t)
	if _, err := tenant.Execute(context.Background(), "", "browsestatus", nil); err == nil {
		t.Fatal("anonymous operational status exposed")
	}
	value, err := tenant.Execute(context.Background(), tenant.Policy().Owner, "browsestatus", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	for _, field := range []string{"capabilities", "git_sync", "storage", "jobs", "delivery", "backups"} {
		if !strings.Contains(string(raw), `"`+field+`"`) {
			t.Fatalf("missing %s: %s", field, raw)
		}
	}
}

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/blob"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
)

func TestMountedSiteHostIsolatedPerTenant(t *testing.T) {
	ctx := context.Background()
	ownerSecret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(ownerSecret)
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(ctx, Config{DataDir: t.TempDir(), DefaultTenant: "alice", PublicURL: "https://relay.test"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	var tenants []*Tenant
	for _, name := range []string{"alice", "bob"} {
		meta, err := app.Create(ctx, CreateOptions{Name: name, Owner: owner, Template: "default"})
		if err != nil {
			t.Fatal(err)
		}
		tenant, err := app.tenant(ctx, meta, "https://relay.test")
		if err != nil {
			t.Fatal(err)
		}
		tenants = append(tenants, tenant)
	}
	content := []string{"alice site", "bob site"}
	label := ""
	for i, tenant := range tenants {
		hashBytes := sha256.Sum256([]byte(content[i]))
		hash := hex.EncodeToString(hashBytes[:])
		if _, err := tenant.blobs.Put(ctx, blobPut(content[i], owner)); err != nil {
			t.Fatal(err)
		}
		e := event.Event{Kind: sites.KindSite, CreatedAt: 1, Tags: [][]string{{"path", "/index.html", hash}}}
		if err := event.Sign(&e, ownerSecret); err != nil {
			t.Fatal(err)
		}
		if _, err := tenant.Publish(ctx, e, relay.Session{PubKeys: []string{owner}}); err != nil {
			t.Fatal(err)
		}
		if label == "" {
			label = sites.SiteLabel(e)
		}
	}
	for i, name := range []string{"alice", "bob"} {
		req := httptest.NewRequest(http.MethodGet, "https://"+label+"."+name+".relay.test/", nil)
		res := httptest.NewRecorder()
		app.ServeHTTP(res, req)
		if res.Code != http.StatusOK || res.Body.String() != content[i] {
			t.Fatalf("tenant %s site status=%d body=%q", name, res.Code, res.Body.String())
		}
	}
	// NIP-AD discovery is mounted through the tenant router and must only
	// reveal an event held by that tenant.
	note := event.Event{Kind: 1, CreatedAt: 2, Content: "alice-only"}
	if err := event.Sign(&note, ownerSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenants[0].Publish(ctx, note, relay.Session{PubKeys: []string{owner}}); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"alice", "bob"} {
		requestURL := "https://relay.test/.well-known/nostr.json?path=/e/" + note.ID
		if name == "bob" {
			requestURL = "https://relay.test/r/bob/.well-known/nostr.json?path=/e/" + note.ID
		}
		req := httptest.NewRequest(http.MethodGet, requestURL, nil)
		signRequest(t, req, "")
		res := httptest.NewRecorder()
		app.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("NIP-AD %s status=%d body=%s", name, res.Code, res.Body.String())
		}
		var doc map[string]struct {
			Filter map[string]any `json:"filter"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		_, found := doc["/e/"+note.ID]
		if (i == 0) != found {
			t.Fatalf("NIP-AD %s found=%v document=%s", name, found, res.Body.String())
		}
		if i == 0 && len(doc["/e/"+note.ID].Filter) == 0 {
			t.Fatal("NIP-AD positive response omitted filter")
		}
	}
	meta, err := app.Catalog().GetByName(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Catalog().AddHost(ctx, meta.ID, "sites.example.test", label); err != nil {
		t.Fatal(err)
	}
	custom := httptest.NewRequest(http.MethodGet, "https://sites.example.test/", nil)
	customRes := httptest.NewRecorder()
	app.ServeHTTP(customRes, custom)
	if customRes.Code != http.StatusOK || customRes.Body.String() != content[0] {
		t.Fatalf("custom host status=%d body=%q", customRes.Code, customRes.Body.String())
	}
	unknown := httptest.NewRequest(http.MethodGet, "https://"+label+".unknown.relay.test/", nil)
	res := httptest.NewRecorder()
	app.ServeHTTP(res, unknown)
	if res.Code == http.StatusOK && strings.Contains(res.Body.String(), "alice site") {
		t.Fatal("unknown tenant served another tenant site")
	}
}

func blobPut(content, uploader string) blob.PutOptions {
	return blob.PutOptions{Reader: strings.NewReader(content), Type: "text/html", Uploader: uploader}
}

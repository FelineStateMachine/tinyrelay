package daemon

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/catalog"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
)

func TestTenantScopedSiteHostIsolation(t *testing.T) {
	ctx := context.Background()
	app, err := New(ctx, Config{DataDir: t.TempDir(), DefaultTenant: "alice", PublicURL: "https://relay.test"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	owner := strings.Repeat("a", 64)
	alice, err := app.Create(ctx, CreateOptions{Name: "alice", Owner: owner, Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Create(ctx, CreateOptions{Name: "bob", Owner: owner, Template: "default"}); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("b", 64)
	label, err := sites.Base36(key)
	if err != nil {
		t.Fatal(err)
	}
	label += "docs"
	for _, tc := range []struct{ host, want string }{{label + ".alice.relay.test", alice.Name}, {label + ".bob.relay.test", "bob"}} {
		r := httptest.NewRequest("GET", "https://"+tc.host+"/", nil)
		meta, gotLabel, found, err := app.resolveHostedSite(r)
		if err != nil || !found {
			t.Fatalf("%s: found=%v err=%v", tc.host, found, err)
		}
		if meta.Name != tc.want || gotLabel != label {
			t.Fatalf("%s: meta=%s label=%s", tc.host, meta.Name, gotLabel)
		}
	}
}

func TestCustomSiteHostMapsOnlyConfiguredTenant(t *testing.T) {
	ctx := context.Background()
	app, err := New(ctx, Config{DataDir: t.TempDir(), DefaultTenant: "alice", PublicURL: "https://relay.test"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	owner := strings.Repeat("a", 64)
	meta, err := app.Create(ctx, CreateOptions{Name: "alice", Owner: owner, Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("c", 64)
	label, _ := sites.Base36(key)
	label += "x"
	if _, err := app.Catalog().AddHost(ctx, meta.ID, "sites.example.test", label); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "https://sites.example.test/", nil)
	got, gotLabel, found, err := app.resolveHostedSite(r)
	if err != nil || !found {
		t.Fatalf("custom host: found=%v err=%v", found, err)
	}
	if got.ID != meta.ID || gotLabel != label {
		t.Fatalf("custom host mapped to %s/%s", got.ID, gotLabel)
	}
}

func TestUnknownSiteHostNeverFallsBackToDefaultRelay(t *testing.T) {
	ctx := context.Background()
	app, err := New(ctx, Config{DataDir: t.TempDir(), DefaultTenant: "alice", PublicURL: "https://relay.test"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	owner := strings.Repeat("a", 64)
	if _, err := app.Create(ctx, CreateOptions{Name: "alice", Owner: owner, Template: "default"}); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("d", 64)
	label, err := sites.Base36(key)
	if err != nil {
		t.Fatal(err)
	}
	label += "missing"
	r := httptest.NewRequest("GET", "https://"+label+".unknown.relay.test/", nil)
	_, _, found, err := app.resolveHostedSite(r)
	if !found || !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("unknown tenant: found=%v err=%v", found, err)
	}
}

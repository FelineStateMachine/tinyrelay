package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestVisibleConnectionsFollowViewerRole(t *testing.T) {
	ctx := context.Background()
	_, tenant := testTenant(t)
	rows := `[{"template":"notes","visibility":"public"},{"template":"bookmarks","visibility":"members"},{"template":"relay-page","visibility":"owner"},{"template":"nope","visibility":"public"}]`
	if _, err := tenant.Execute(ctx, tenant.Policy().Owner, "setconnections", []json.RawMessage{json.RawMessage(rows)}); err != nil {
		t.Fatal(err)
	}
	names := func(actor string) string {
		value, err := tenant.Execute(ctx, actor, "connections", nil)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(value)
		var list []map[string]any
		_ = json.Unmarshal(raw, &list)
		out := []string{}
		for _, item := range list {
			out = append(out, item["template"].(string))
		}
		return strings.Join(out, ",")
	}
	if got := names(""); got != "notes" {
		t.Fatalf("guest sees %q", got)
	}
	if got := names(tenant.Policy().Owner); got != "notes,bookmarks,relay-page" {
		t.Fatalf("owner sees %q", got)
	}
	value, _ := tenant.Execute(ctx, "", "connections", nil)
	raw, _ := json.Marshal(value)
	if !strings.Contains(string(raw), "jumble.social") || !strings.Contains(string(raw), `"links"`) {
		t.Fatalf("connection is missing its template links: %s", raw)
	}
}

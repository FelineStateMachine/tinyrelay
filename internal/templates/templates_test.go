package templates

import "testing"

func TestCatalogShapeAndTemplateApplication(t *testing.T) {
	if got := len(Names()); got != 16 {
		t.Fatalf("template count = %d, want 16", got)
	}
	if got := len(Connections()); got != 12 {
		t.Fatalf("connection count = %d, want 12", got)
	}
	p, err := ApplyTemplate("outbox", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if p.Owner != "owner" || p.Writes != "owner" || !p.Delivery.Enabled || len(p.BlockedKinds) != 3 {
		t.Fatalf("outbox application: %#v", p)
	}
	social, err := ApplyTemplate("social", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if social.Owner != "owner" || social.Writes != "allowlist" || social.Reads != "open" {
		t.Fatalf("social policy: %#v", social)
	}
	for _, kind := range []int{0, 1, 5, 6, 7, 1111, 30023} {
		if !containsKind(social.AllowedKinds, kind) {
			t.Fatalf("social does not allow kind %d: %#v", kind, social.AllowedKinds)
		}
	}
	if _, err := ApplyTemplate("missing", "owner"); err == nil {
		t.Fatal("missing template unexpectedly applied")
	}
	for _, name := range Names() {
		if _, err := ApplyTemplate(name, "owner"); err != nil {
			t.Fatalf("template %s: %v", name, err)
		}
	}
}

func containsKind(kinds []int, want int) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}

package templates

import "testing"

func TestCatalogShapeAndTemplateApplication(t *testing.T) {
	if got := len(Names()); got != 15 {
		t.Fatalf("template count = %d, want 15", got)
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
	if _, err := ApplyTemplate("missing", "owner"); err == nil {
		t.Fatal("missing template unexpectedly applied")
	}
	for _, name := range Names() {
		if _, err := ApplyTemplate(name, "owner"); err != nil {
			t.Fatalf("template %s: %v", name, err)
		}
	}
}

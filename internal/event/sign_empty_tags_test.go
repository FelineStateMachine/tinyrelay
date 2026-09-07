package event

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSignEmptyTagsProducesValidWireEvent(t *testing.T) {
	e := Event{Kind: 0, CreatedAt: 1, Content: "{}"}
	if err := Sign(&e, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw); err != nil {
		t.Fatalf("signed event rejected: %s: %v", raw, err)
	}
	if !strings.Contains(string(raw), `"tags":[]`) {
		t.Fatalf("tags must be an array: %s", raw)
	}
}

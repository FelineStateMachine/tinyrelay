package nostr

import (
	"strings"
	"testing"
)

func TestParseFilterRejectsNonObjectsAndNullFields(t *testing.T) {
	for _, raw := range []string{"null", "[]", `{"since":null}`, `{"until":null}`, `{"limit":null}`, `{"search":null}`, `{"ids":[null]}`, `{"authors":[null]}`, `{"kinds":[null]}`, `{"#t":[null]}`} {
		if _, err := ParseFilter([]byte(raw)); err == nil {
			t.Errorf("accepted invalid filter %s", raw)
		}
	}
}

// FuzzParse exercises the complete event wire boundary with arbitrary input.
// Parse must reject malformed input without panicking.
func FuzzParse(f *testing.F) {
	f.Add([]byte(`{"id":"","pubkey":"","created_at":0,"kind":1,"tags":[],"content":"","sig":""}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"id":1,"tags":null}`))
	seed := Event{CreatedAt: 1_700_000_000, Kind: 1, Tags: [][]string{{"t", "seed"}}, Content: "valid seed"}
	if err := Sign(&seed, strings.Repeat("0", 63)+"1"); err != nil {
		f.Fatal(err)
	}
	encoded, err := Canonical(seed)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(encoded)
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = Parse(raw)
	})
}

// FuzzParseFilter covers both ordinary and #tag filter keys and checks that
// accepted filters remain safe to marshal and parse again.
func FuzzParseFilter(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"#t":["go"],"kinds":[1],"limit":10}`))
	f.Add([]byte(`{"ids":null,"since":-1,"search":""}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		filter, err := ParseFilter(raw)
		if err != nil {
			return
		}
		encoded, err := filter.MarshalJSON()
		if err != nil {
			t.Fatalf("marshal accepted filter: %v", err)
		}
		if _, err := ParseFilter(encoded); err != nil {
			t.Fatalf("accepted filter did not round trip: %v", err)
		}
	})
}

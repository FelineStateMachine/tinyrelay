package event

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestSignParseValidateCanonical(t *testing.T) {
	secret := strings.Repeat("0", 63) + "1"
	e := Event{CreatedAt: 1, Kind: 1, Tags: [][]string{{"t", "go"}}, Content: "hello"}
	if err := Sign(&e, secret); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := Validate(e); err != nil {
		t.Fatalf("validate: %v", err)
	}
	got, err := Parse(mustJSON(t, e))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	canonical, err := Canonical(e)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	if string(canonical) != string(mustJSON(t, got)) {
		t.Fatalf("canonical changed event: %s", canonical)
	}
}

func TestFilterTagJSONAndEmptyLists(t *testing.T) {
	f, err := ParseFilter([]byte(`{"#t":["go"],"kinds":[],"limit":0}`))
	if err != nil {
		t.Fatalf("parse filter: %v", err)
	}
	if len(f.Tags["t"]) != 1 || len(f.Kinds) != 0 || f.Kinds == nil {
		t.Fatalf("filter fields lost: %#v", f)
	}
	e := Event{ID: strings.Repeat("a", 64), PubKey: strings.Repeat("b", 64), Kind: 1, Tags: [][]string{{"t", "go"}}}
	if Matches(f, e) {
		t.Fatal("empty kinds must match nothing")
	}
	f.Kinds = nil
	if !Matches(f, e) {
		t.Fatal("tag filter should match")
	}
	raw, err := json.Marshal(f)
	if err != nil || !strings.Contains(string(raw), `"#t"`) {
		t.Fatalf("marshal tags: %s (%v)", raw, err)
	}
}

func TestSearchTermsAndKinds(t *testing.T) {
	terms := SearchTerms("hello lang:en world:")
	if len(terms) != 1 || terms[0] != "hello" {
		t.Fatalf("terms: %#v", terms)
	}
	if !IsEphemeral(20001) || IsEphemeral(30000) || !IsReplaceable(10001) || !IsAddressable(30001) || !IsPrivate(4) || IsPrivate(KIND_DM) {
		t.Fatal("kind classification mismatch")
	}
}

func TestNIP01CanonicalEscapingMatchesJSONStringify(t *testing.T) {
	e := Event{CreatedAt: 1700000000, Kind: 1, Tags: [][]string{{"x", "a\b\f\u2028\u2029é"}}, Content: "quote \" slash \\ newline\n <tag>"}
	got, err := serializeForID(e)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte(`\u003c`)) || !bytes.Contains(got, []byte(`\b`)) || !bytes.Contains(got, []byte(`\f`)) {
		t.Fatalf("unexpected HTML/control escaping: %s", got)
	}
	if strings.Contains(string(got), `\u2028`) || strings.Contains(string(got), `\u2029`) {
		t.Fatalf("U+2028/U+2029 must remain literal like JSON.stringify: %s", got)
	}
}

func TestParseRequiresEveryWireField(t *testing.T) {
	e := Event{CreatedAt: 1, Kind: 1, Tags: [][]string{}, Content: ""}
	if err := Sign(&e, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"id", "pubkey", "created_at", "kind", "tags", "content", "sig"} {
		var values map[string]json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			t.Fatal(err)
		}
		delete(values, field)
		missing, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(missing); err == nil {
			t.Errorf("missing %s accepted", field)
		}
	}
}

func TestNIP01IDIsSHA256OfCanonicalArray(t *testing.T) {
	e := Event{CreatedAt: 1, Kind: 1, Tags: [][]string{}, Content: "hello"}
	if err := Sign(&e, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	serialized, err := serializeForID(e)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(serialized)
	if e.ID != hex.EncodeToString(sum[:]) {
		t.Fatalf("id %s does not hash canonical array %x", e.ID, sum)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

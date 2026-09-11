package seedmark

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestVersionOneMatchesUpstreamSVG pins the v1 output from Seedmark commit
// c808a940bc193f97512c02d58da33ecc28ba1f53. Seedmark is MIT licensed; its
// Go port and upstream license are in this package. The fixture generator is
// scripts/qa/generate-seedmark-fixture.mjs.
func TestVersionOneMatchesUpstreamSVG(t *testing.T) {
	raw, err := os.ReadFile("testdata/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct{ Seed, Family, SHA256 string }
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	families := map[string]bool{}
	if len(fixtures) != 1010 {
		t.Fatalf("fixtures contain %d cases, want 1010", len(fixtures))
	}
	for _, fixture := range fixtures {
		families[fixture.Family] = true
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(Avatar(fixture.Seed))))
		if digest != fixture.SHA256 {
			t.Fatalf("seed %q (%s): got SHA256 %s, want %s", fixture.Seed, fixture.Family, digest, fixture.SHA256)
		}
	}
	if len(families) != 8 {
		t.Fatalf("fixtures cover %d families", len(families))
	}
}

func TestSeedNeverAppearsInMarkup(t *testing.T) {
	for _, seed := range []string{`<script>alert(1)</script>`, `" onload="alert(1)`, "https://example.test/identity"} {
		svg := Avatar(seed)
		if strings.Contains(svg, seed) || strings.Contains(svg, "<script") || strings.Contains(svg, "onload") || strings.Contains(svg, "href=") || strings.Contains(svg, "class=") {
			t.Fatalf("seed affected SVG markup: %s", svg)
		}
	}
}

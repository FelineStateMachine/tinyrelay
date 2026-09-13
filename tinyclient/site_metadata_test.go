package tinyclient

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
)

func TestSitePresentationMatchesRelayManifests(t *testing.T) {
	for _, kind := range []int{15128, 35128, 5128, 1} {
		for _, key := range []string{strings.Repeat("0", 63) + "1", strings.Repeat("f", 64), "invalid"} {
			e := event.Event{Kind: kind, PubKey: key, ID: key, Tags: [][]string{{"d", "demo"}, {"path", "/index.html", "hash"}, {"path"}, {"other"}}}
			if got, want := siteLabel(e), sites.SiteLabel(e); got != want {
				t.Errorf("label kind=%d key=%s: %s want %s", kind, key, got, want)
			}
			if got, want := sitePathCount(e), len(sites.SitePaths(e)); got != want {
				t.Errorf("path count=%d want %d", got, want)
			}
		}
	}
}

func TestFrontendBuildDoesNotLinkRelayStorage(t *testing.T) {
	command := exec.Command("go", "list", "-deps", "github.com/FelineStateMachine/tinyrelay/cmd/tinyclient")
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("list frontend dependencies: %v %s", err, out)
	}
	for _, dependency := range strings.Fields(string(out)) {
		if strings.Contains(dependency, "/internal/storage") || strings.Contains(dependency, "/internal/daemon") || strings.Contains(dependency, "modernc.org/sqlite") || strings.HasSuffix(dependency, "/tinygit") {
			t.Errorf("frontend links backend implementation %s", dependency)
		}
	}
}

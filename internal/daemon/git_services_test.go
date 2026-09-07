package daemon

import (
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
)

func TestGitEventFiltersCoverRepositoryAndCollaborationScopes(t *testing.T) {
	owner := strings.Repeat("a", 64)
	filters := gitEventFilters(gitrelay.Repository{Owner: owner, Identifier: "repo", Maintainers: []string{strings.Repeat("b", 64)}})
	if len(filters) != 3 {
		t.Fatalf("filter count = %d", len(filters))
	}
	if len(filters[0].Authors) != 2 || filters[0].Tags["d"][0] != "repo" {
		t.Fatalf("metadata filter = %#v", filters[0])
	}
	for _, filter := range filters[1:] {
		values := filter.Tags["a"]
		if len(values) == 0 {
			values = filter.Tags["A"]
		}
		if len(filter.Kinds) != 9 || len(values) != 1 || values[0] != "30617:"+owner+":repo" {
			t.Fatalf("collaboration filter = %#v", filter)
		}
	}
}

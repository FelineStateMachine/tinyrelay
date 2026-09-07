package daemon

import (
	"encoding/json"
	"testing"
)

func TestAddressFilterForPath(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"event", "/e/" + id, true},
		{"article", "/a/article-name", true},
		{"view", "/view/profiles", true},
		{"bad event", "/e/not-an-id", false},
		{"traversal", "/view/%2fetc", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filter, ok := AddressFilterForPath(test.path, id, id)
			if ok != test.want {
				t.Fatalf("ok = %v, want %v (filter=%v)", ok, test.want, filter)
			}
		})
	}
}

func TestAddressFilterForPathShape(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	filter, ok := AddressFilterForPath("/a/my%20article", id, id)
	if !ok || filter["#d"].([]string)[0] != "my article" {
		t.Fatalf("unexpected address filter: %#v", filter)
	}
	filter, ok = AddressFilterForPath("/view/profiles", id, id)
	if !ok || filter["#d"].([]string)[0] != "bind.ws/view/profiles" {
		t.Fatalf("unexpected view filter: %#v", filter)
	}
}

func TestPublicFilterDefaultsAndLimit(t *testing.T) {
	f, err := publicFilter(nil)
	if err != nil || f.Limit == nil || *f.Limit != 50 {
		t.Fatalf("default filter: %#v %v", f, err)
	}
	raw, _ := json.Marshal(map[string]any{"kinds": []int{1}, "limit": 1000})
	f, err = publicFilter([]json.RawMessage{raw})
	if err != nil || f.Limit == nil || *f.Limit != 1000 {
		t.Fatalf("bounded filter: %#v %v", f, err)
	}
}

func TestManagementMethodsExcludeHostedControls(t *testing.T) {
	for _, method := range ManagementMethods() {
		for _, excluded := range []string{"lease", "trial", "fuel", "productcapacity"} {
			if method == excluded {
				t.Fatalf("hosted control %q leaked into self-hosted registry", method)
			}
		}
	}
}

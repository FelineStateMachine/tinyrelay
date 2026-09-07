package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestManagementGoldenInventory is intentionally a diagnostic inventory. It
// records source methods that still return unsupported after the complete
// tenant dispatcher has been constructed; package tests must not treat a
// static registry as implementation proof.
func TestManagementGoldenInventory(t *testing.T) {
	params := map[string][]json.RawMessage{}
	for _, method := range ManagementMethods() {
		params[method] = nil
	}
	params["setpolicy"] = []json.RawMessage{json.RawMessage(`{}`)}
	params["setconnections"] = []json.RawMessage{json.RawMessage(`[]`)}
	params["deleteevent"] = []json.RawMessage{json.RawMessage(`"` + strings.Repeat("0", 64) + `"`)}
	params["deleteblob"] = []json.RawMessage{json.RawMessage(`"` + strings.Repeat("0", 64) + `"`)}
	params["deleterelay"] = []json.RawMessage{json.RawMessage(`"main"`)}
	params["forkrelay"] = []json.RawMessage{json.RawMessage(`{"name":"fork-golden"}`)}
	params["transferowner"] = []json.RawMessage{json.RawMessage(`"` + strings.Repeat("1", 64) + `"`)}
	readMethods := map[string]bool{}
	for _, method := range []string{"supportedmethods", "listaudit", "stats", "getpolicy", "listviews", "listbannedpubkeys", "listallowedpubkeys", "listmembers", "listpeople", "listinvites", "listclaims", "listlisthistory", "listeventsneedingmoderation", "listblockedips", "listreports", "exportconfig", "listblobs", "listsites", "listrecentevents", "searchevents", "listpins", "storagestats", "listretention", "listallowedkinds", "listblockedkinds", "listpresets", "listconnectiontemplates", "listconnections", "pullstatus", "listjobs", "deliverystatus", "listdumps", "listbackups", "clearsuccession", "successionstatus", "listdomains"} {
		readMethods[method] = true
	}
	for _, method := range ManagementMethods() {
		t.Run(method, func(t *testing.T) {
			_, tenant := testTenant(t)
			_, err := tenant.Execute(context.Background(), tenant.Policy().Owner, method, params[method])
			if err != nil && strings.HasPrefix(err.Error(), "unsupported:") {
				t.Errorf("missing %s: %v", method, err)
			}
			if readMethods[method] && err != nil {
				t.Errorf("read method %s failed: %v", method, err)
			}
		})
	}
}

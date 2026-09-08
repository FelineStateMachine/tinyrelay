package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestPrivateNIP11MatchesRuntimeGRASPCapabilities(t *testing.T) {
	_, tenant := testTenant(t)
	p := tenant.Policy()
	p.Features.Grasp = true
	p.Features.Grasp02 = true
	p.Features.Grasp03 = true
	p.Features.Grasp06 = true
	p.Features.Grasp08 = true
	p.Reads = "members"
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://relay.test/", nil)
	req.Header.Set("Accept", "application/nostr+json")
	res := httptest.NewRecorder()
	tenant.ServeHTTP(res, req)
	var info struct {
		Profiles []string `json:"supported_grasps"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	want := []string{"GRASP-01", "GRASP-02", "GRASP-03", "GRASP-06", "GRASP-08"}
	if !reflect.DeepEqual(info.Profiles, want) {
		t.Fatalf("NIP-11 profiles = %v", info.Profiles)
	}
	for _, capability := range tenant.Capabilities(nil) {
		if capability.ID == "GRASP" && !reflect.DeepEqual(capability.Profiles, info.Profiles) {
			t.Fatalf("runtime profiles = %v; NIP-11 = %v", capability.Profiles, info.Profiles)
		}
	}
}

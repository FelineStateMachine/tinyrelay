package daemon

import (
	"net/http"
	"strings"
	"testing"
)

func TestBlobGetMissingReturnsNotFoundAfterAuthentication(t *testing.T) {
	_, tenant := testTenant(t)
	missing := strings.Repeat("a", 64)

	response := getArtifact(t, tenant, "/"+missing, testOwnerSecret)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing authenticated blob status = %d, want %d: %s", response.Code, http.StatusNotFound, response.Body.String())
	}
}

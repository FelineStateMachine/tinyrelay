package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestLLMSDocumentsRegisteredToolsAndRoutes(t *testing.T) {
	app, tenant := testTenant(t)
	for _, prefix := range []string{"", "/r/main"} {
		t.Run(prefix, func(t *testing.T) {
			response := httptest.NewRecorder()
			app.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://relay.test"+prefix+"/llms.txt?v=docs", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("llms.txt status: %d", response.Code)
			}
			body := response.Body.String()
			words := map[string]bool{}
			for _, word := range regexp.MustCompile(`[a-z_]+`).FindAllString(body, -1) {
				words[word] = true
			}
			for _, tool := range tenant.mcp.Tools.Tools() {
				if !words[tool.Name] {
					t.Errorf("registered tool %q is missing from llms.txt", tool.Name)
				}
			}
			for _, path := range []string{"/mcp", "/chat", "/social", "/account", "/approvals", "/files", "/wiki"} {
				if !strings.Contains(body, "http://relay.test"+prefix+path) {
					t.Errorf("missing tenant-aware route %s", path)
				}
			}
			if strings.Contains(body, "?v=docs") {
				t.Error("cache-busting query leaked into documentation URLs")
			}
			links := regexp.MustCompile(`https://github.com/FelineStateMachine/tinyrelay/blob/main/(docs/[a-z-]+\.md)`).FindAllStringSubmatch(body, -1)
			if len(links) < 8 {
				t.Errorf("expected feature guides, got %d documentation links", len(links))
			}
			for _, link := range links {
				if _, err := os.Stat("../../" + link[1]); err != nil {
					t.Errorf("broken guide link %s: %v", link[1], err)
				}
			}
		})
	}
}

func TestLLMSKeepsPrivatePresentationOutOfPublicGuide(t *testing.T) {
	app, tenant := testTenant(t)
	policy := tenant.Policy()
	policy.Features.Grasp = true
	policy.Features.Grasp08 = true
	policy.Reads = "members"
	policy.Name, policy.Description = "secret-name", "secret-description"
	if err := tenant.applyPolicy(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	app.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://relay.test/llms.txt", nil))
	body := response.Body.String()
	if response.Code != http.StatusOK || strings.Contains(body, "secret-name") || strings.Contains(body, "secret-description") {
		t.Fatalf("private guide status=%d body=%s", response.Code, body)
	}
}

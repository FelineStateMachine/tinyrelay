package webui

import (
	"net/http"
	"strings"
)

// handleControlRoute dispatches endpoints that mutate or stream management
// state. Keeping these together makes their authentication and HTTP method
// requirements visible without mixing them into page and asset routing.
func (a *App) handleControlRoute(writer http.ResponseWriter, request *http.Request) bool {
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/manage/rpc":
		a.rpc(writer, request)
	case request.Method == http.MethodGet && request.URL.Path == "/api/join-policy":
		a.joinPolicy(writer)
	case request.Method == http.MethodPost && request.URL.Path == "/api/invites/claim":
		a.claimInvite(writer, request)
	case request.Method == http.MethodGet && request.URL.Path == "/manage/jobs/status":
		a.jobStatus(writer, request)
	default:
		return false
	}
	return true
}

// handleBrowseRoute dispatches authenticated browse pages and compatibility
// redirects. API and asset endpoints remain in the public route group.
func (a *App) handleBrowseRoute(writer http.ResponseWriter, request *http.Request) bool {
	if request.URL.Path == "/manage/status" || request.URL.Path == "/tools" {
		http.Redirect(writer, request, requestPrefix(request)+"/manage/health", http.StatusMovedPermanently)
		return true
	}
	if request.Method == http.MethodGet && isBrowsePath(request.URL.Path) {
		a.browse(writer, request)
		return true
	}
	if request.URL.Path == "/share" {
		http.Redirect(writer, request, requestPrefix(request)+"/files", http.StatusSeeOther)
		return true
	}
	return false
}

func isBrowsePath(path string) bool {
	return path == "/repos" || path == "/repo" || path == "/files" || path == "/file" ||
		path == "/approvals" || path == "/profile" || path == "/wiki" ||
		strings.HasPrefix(path, "/wiki/") || path == "/manage/health" || roomRoute(path).tab != ""
}

// handleWebMCPRoute dispatches the browser-facing query bridge separately
// from HTML pages so its contract can evolve independently.
func (a *App) handleWebMCPRoute(writer http.ResponseWriter, request *http.Request) bool {
	if request.Method != http.MethodGet {
		return false
	}
	switch request.URL.Path {
	case "/webmcp.js":
		a.webMCP(writer, request)
	case "/webmcp/query":
		a.webMCPQuery(writer, request)
	default:
		return false
	}
	return true
}

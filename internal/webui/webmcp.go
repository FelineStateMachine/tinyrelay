package webui

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
)

//go:embed webmcp.js
var webMCPJavaScript string

func (a *App) webMCP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("content-type", "application/javascript; charset=utf-8")
	writer.Header().Set("cache-control", "no-store")
	_, _ = writer.Write([]byte(webMCPJavaScript))
}

func (a *App) webMCPQuery(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("cache-control", "no-store, private")
	method := request.URL.Query().Get("method")
	var params []json.RawMessage
	if raw := request.URL.Query().Get("params"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &params); err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid WebMCP query"})
			return
		}
	}
	allowed := map[string]bool{"browserepos": true, "browserepo": true, "browsefiles": true, "browsefile": true, "browsestatus": true, "browseissues": true, "browsepulls": true, "browseissue": true, "browsepull": true, "browsewiki": true, "browsewikipage": true, "browsewikimerge": true, "browserooms": true, "browseroom": true, "browsethread": true}
	if !allowed[method] {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "unsupported WebMCP query"})
		return
	}
	actor, err := a.resolveActor(request)
	if err != nil {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	result, err := a.backend.Query(request.Context(), method, params, actor)
	if err != nil {
		writeJSON(writer, http.StatusForbidden, map[string]string{"error": fmt.Sprint(err)})
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

var _ http.Handler = (*App)(nil)

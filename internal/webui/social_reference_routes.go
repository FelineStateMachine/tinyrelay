package webui

import (
	"encoding/json"
	"net/http"
	"strings"
)

// socialReferencePage tries the Social renderer for NIP-19 event references.
// The generic event page remains the fallback for event kinds that Social does
// not publish or for backends that predate the Social browse methods.
func (a *App) socialReferencePage(writer http.ResponseWriter, request *http.Request, identifier string, address bool, actor string) bool {
	if !a.backend.Policy().Features.Pages {
		return false
	}
	if address && !socialArticleAddress(identifier) {
		return false
	}
	params := map[string]any{"limit": 30}
	if cursor := request.URL.Query().Get("cursor"); cursor != "" {
		params["cursor"] = cursor
	}
	if address {
		params["address"] = identifier
	} else {
		params["id"] = identifier
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return false
	}
	result, err := a.backend.Query(request.Context(), "browsesocialthread", []json.RawMessage{raw}, actor)
	if err != nil || !socialThreadPost(result) {
		return false
	}
	data := PageData{
		Event: result,
		Query: request.URL.Query(),
		Tab:   "social-thread",
		Title: "Social | " + a.backend.Slug(),
	}
	a.render(writer, request, data)
	return true
}

func socialArticleAddress(value string) bool {
	parts := strings.SplitN(value, ":", 3)
	return len(parts) == 3 && parts[0] == "30023" && hexID.MatchString(parts[1])
}

func socialThreadPost(value any) bool {
	post, ok := valueMap(value)["post"]
	if !ok {
		return false
	}
	row := valueMap(post)
	kind := int(unixSeconds(row["kind"]))
	return kind == 1 || kind == 1111 || kind == 30023
}

package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func (a *App) handleSocialRoute(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	if p != "/articles" && p != "/social" && p != "/social.json" && p != "/social.xml" && !strings.HasPrefix(p, "/social/") {
		return false
	}
	if !a.backend.Policy().Features.Pages {
		http.NotFound(w, r)
		return true
	}
	if p == "/social/preview" && r.Method == http.MethodPost {
		a.socialPreviewPage(w, r)
		return true
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	if p == "/articles" {
		target := requestPrefix(r) + "/social"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
		return true
	}
	if p == "/social.json" || p == "/social.xml" {
		a.socialSyndication(w, r)
		return true
	}
	a.socialPage(w, r)
	return true
}

func socialParameters(q url.Values) map[string]any {
	params := map[string]any{"limit": 30}
	for _, key := range []string{"kind", "author", "q", "cursor", "address"} {
		if value := q.Get(key); value != "" {
			params[key] = value
		}
	}
	if q.Get("kind") == "" {
		switch q.Get("kinds") {
		case "1":
			params["kind"] = "notes"
		case "30023":
			params["kind"] = "articles"
		}
	}
	return params
}

func (a *App) socialPage(w http.ResponseWriter, r *http.Request) {
	actor, _ := a.resolveActor(r)
	params := socialParameters(r.URL.Query())
	method, tab := "browsesocial", "social"
	if strings.HasPrefix(r.URL.Path, "/social/") {
		params["id"] = strings.TrimPrefix(r.URL.Path, "/social/")
		method, tab = "browsesocialthread", "social-thread"
	} else if r.URL.Query().Get("address") != "" {
		method, tab = "browsesocialthread", "social-thread"
	}
	raw, err := json.Marshal(params)
	if err != nil {
		http.Error(w, "invalid social query", http.StatusBadRequest)
		return
	}
	result, err := a.backend.Query(r.Context(), method, []json.RawMessage{raw}, actor)
	data := PageData{Tab: tab, Title: "Social | " + a.backend.Slug(), Event: result, Query: r.URL.Query()}
	if tab == "social" {
		data.Feed = browseRows(result)
	}
	if err != nil {
		data.Error = err.Error()
		if strings.HasPrefix(err.Error(), "not found:") {
			w.WriteHeader(http.StatusNotFound)
		}
	}
	a.render(w, r, data)
}

func (a *App) socialPreviewPage(w http.ResponseWriter, r *http.Request) {
	actor, err := a.resolveActor(r)
	if err != nil || actor == "" {
		http.Error(w, "Sign in to preview a draft.", http.StatusUnauthorized)
		return
	}
	var input struct {
		Content string `json:"content"`
		Kind    int    `json:"kind"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		http.Error(w, "Invalid preview.", http.StatusBadRequest)
		return
	}
	if decoder.Decode(new(any)) != io.EOF || (input.Kind != 1 && input.Kind != 30023 && input.Kind != 1111) {
		http.Error(w, "Invalid preview.", http.StatusBadRequest)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, map[string]any{"html": a.socialBody(map[string]any{"content": input.Content, "kind": input.Kind})})
}

func socialContext(value any, data PageData) map[string]any {
	row := valueMap(value)
	out := make(map[string]any, len(row)+4)
	for key, value := range row {
		out[key] = value
	}
	out["viewer"] = data.Actor
	out["detail"] = data.Tab == "social-thread"
	out["relay"] = wsURL(data.URL)
	out["thread"] = valueMap(data.Event)["post"]
	out["parent_url"] = socialReferenceURL(row, "parent_")
	out["root_url"] = socialReferenceURL(row, "root_")
	parent := plainString(row["parent_id"])
	thread := valueMap(out["thread"])
	out["nested"] = parent != "" && parent != plainString(thread["id"])
	return out
}

func socialReferenceURL(row map[string]any, prefix string) string {
	if address := plainString(row[prefix+"address"]); strings.HasPrefix(address, "30023:") {
		return "/social?address=" + url.QueryEscape(address)
	}
	if id := plainString(row[prefix+"id"]); hexID.MatchString(id) {
		return "/social/" + id
	}
	return ""
}

func socialThreadPage(path string, query url.Values, cursor string) string {
	next := url.Values{}
	if address := query.Get("address"); address != "" {
		next.Set("address", address)
	}
	next.Set("cursor", cursor)
	return path + "?" + next.Encode()
}

func socialReactionCount(value any, content string) int {
	for _, row := range browseRows(valueMap(value)["reactions"]) {
		if plainString(valueMap(row)["content"]) == content {
			return int(unixSeconds(valueMap(row)["count"]))
		}
	}
	return 0
}

func socialReacted(value any, content string) bool {
	for _, row := range browseRows(valueMap(value)["reactions"]) {
		if plainString(valueMap(row)["content"]) == content {
			return valueMap(row)["reacted"] == true
		}
	}
	return false
}

func socialQuery(query url.Values, key, value string) string {
	next := url.Values{}
	for _, name := range []string{"kind", "author", "q", "compose", "address"} {
		if v := query.Get(name); v != "" {
			next.Set(name, v)
		}
	}
	if value == "" {
		next.Del(key)
	} else {
		next.Set(key, value)
	}
	if encoded := next.Encode(); encoded != "" {
		return "/social?" + encoded
	}
	return "/social"
}

func socialURL(value any) string {
	row := valueMap(value)
	if address := plainString(row["address"]); address != "" {
		return "/social?address=" + url.QueryEscape(address)
	}
	return "/social/" + url.PathEscape(plainString(row["id"]))
}

func (a *App) socialSyndication(w http.ResponseWriter, r *http.Request) {
	actor, _ := a.resolveActor(r)
	raw, err := json.Marshal(socialParameters(r.URL.Query()))
	if err != nil {
		http.Error(w, "invalid social query", http.StatusBadRequest)
		return
	}
	result, err := a.backend.Query(r.Context(), "browsesocial", []json.RawMessage{raw}, actor)
	if err != nil {
		http.Error(w, err.Error(), publicErrorStatus(err))
		return
	}
	items := make([]map[string]any, 0)
	for _, row := range browseRows(result) {
		if item, ok := articleItem(row, a.backend.URL()); ok {
			item["url"] = strings.TrimSuffix(a.backend.URL(), "/") + socialURL(row)
			if title := plainString(valueMap(row)["title"]); title != "" {
				item["title"] = title
			}
			items = append(items, item)
		}
	}
	if r.URL.Path == "/social.xml" {
		a.writeAtom(w, items)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": "https://jsonfeed.org/version/1.1", "title": a.backend.Slug() + " Social", "home_page_url": strings.TrimSuffix(a.backend.URL(), "/") + "/social", "feed_url": strings.TrimSuffix(a.backend.URL(), "/") + "/social.json", "items": items})
}

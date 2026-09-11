package webui

import (
	"encoding/json"
	"net/http"
	"strings"
)

func (a *App) handleChatRoute(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if path != "/rooms" && path != "/chat" && !strings.HasPrefix(path, "/chat/") {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	if path == "/rooms" {
		target := requestPrefix(r) + "/chat"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
		return true
	}
	if path == "/chat/activity" {
		a.chatActivityHTTP(w, r)
		return true
	}
	if path == "/chat/events" {
		a.directMessagesJSON(w, r)
		return true
	}
	actor, _ := a.resolveActor(r)
	data := PageData{Tab: "chat", Title: "Chat | " + a.backend.Slug(), Query: r.URL.Query()}
	if path != "/chat" {
		peer, ok := strings.CutPrefix(path, "/chat/dm/")
		if !ok || !eventIDPattern.MatchString(peer) {
			http.NotFound(w, r)
			return true
		}
		data.Tab, data.View, data.Title = "direct", peer, "Direct chat | "+a.backend.Slug()
	}
	if data.Tab == "chat" {
		result, err, typed := a.typedRoomResult(r.Context(), roomPath{tab: "rooms"}, r.URL.Query(), actor)
		if !typed {
			params, _ := json.Marshal(map[string]any{"limit": 100, "cursor": r.URL.Query().Get("cursor")})
			result, err = a.backend.Query(r.Context(), "browserooms", []json.RawMessage{params}, actor)
		}
		data.Event, data.Feed = result, browseRows(result)
		data.Rooms = data.Feed
		if err != nil {
			data.Error = err.Error()
		}
	} else {
		data.Rooms = a.roomList(r.Context(), actor)
	}
	a.render(w, r, data)
	return true
}

func (a *App) directMessagesJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	actor, err := a.resolveActor(r)
	if err != nil || actor == "" {
		http.Error(w, "Sign in to open direct chats.", http.StatusUnauthorized)
		return
	}
	params, _ := json.Marshal(map[string]any{"limit": 50, "cursor": r.URL.Query().Get("cursor")})
	result, err := a.backend.Query(r.Context(), "browsedirectmessages", []json.RawMessage{params}, actor)
	if err != nil {
		http.Error(w, err.Error(), publicErrorStatus(err))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(result)
}

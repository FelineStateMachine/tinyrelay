package tinyclient

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// socialProfileAuthor accepts the relay's canonical hex key and the usual
// Nostr npub spelling, returning the canonical key used by backend queries.
func socialProfileAuthor(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(raw), "npub1") {
		hrp, data, ok := bech32Decode(strings.ToLower(raw))
		if !ok || hrp != "npub" || len(data) != 32 {
			return "", errors.New("invalid npub")
		}
		return hex.EncodeToString(data), nil
	}
	if len(raw) != 64 {
		return "", errors.New("author must be a 64-character public key or npub")
	}
	if _, err := hex.DecodeString(raw); err != nil {
		return "", errors.New("author must be a 64-character public key or npub")
	}
	return strings.ToLower(raw), nil
}

func (a *App) socialProfilePage(w http.ResponseWriter, r *http.Request) {
	actor, actorErr := a.resolveActor(r)
	query := r.URL.Query()
	data := PageData{Tab: "social-profile", Title: "Profile | " + a.backend.Slug(), Query: query}
	if actorErr != nil {
		data.Error = actorErr.Error()
		w.WriteHeader(socialErrorStatus(actorErr))
		a.render(w, r, data)
		return
	}
	author := strings.TrimSpace(query.Get("author"))
	if author == "" {
		author = actor
	}
	if author == "" {
		a.render(w, r, data)
		return
	}
	canonical, err := socialProfileAuthor(author)
	if err != nil {
		data.Error = err.Error()
		w.WriteHeader(http.StatusBadRequest)
		a.render(w, r, data)
		return
	}
	data.View = canonical
	data.Query.Set("author", canonical)

	profileRaw, err := json.Marshal(map[string]any{"pubkey": canonical})
	if err != nil {
		http.Error(w, "invalid profile query", http.StatusBadRequest)
		return
	}
	profile, profileErr := a.backend.Query(r.Context(), "browseprofile", []json.RawMessage{profileRaw}, actor)
	if profileErr != nil {
		data.Error = profileErr.Error()
		w.WriteHeader(socialErrorStatus(profileErr))
		a.render(w, r, data)
		return
	}
	feedParams := socialParameters(data.Query)
	feedParams["author"] = canonical
	feedRaw, marshalErr := json.Marshal(feedParams)
	if marshalErr != nil {
		http.Error(w, "invalid social query", http.StatusBadRequest)
		return
	}
	feed, feedErr := a.backend.Query(r.Context(), "browsesocial", []json.RawMessage{feedRaw}, actor)
	if feedErr != nil {
		data.Error = feedErr.Error()
		w.WriteHeader(socialErrorStatus(feedErr))
	}
	data.Event = map[string]any{
		"profile":     valueMap(profile)["profile"],
		"pubkey":      canonical,
		"next_cursor": valueMap(feed)["next_cursor"],
	}
	data.Feed = browseRows(feed)
	a.render(w, r, data)
}

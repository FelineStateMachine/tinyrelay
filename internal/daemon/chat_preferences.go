package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const chatSharePresenceSetting = "account.chat.share_presence."

type chatPreferences struct {
	SharePresence bool `json:"share_presence"`
}

func chatPreferenceKey(actor string) string {
	return chatSharePresenceSetting + actor
}

func (t *Tenant) chatPreferencesHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	actor, err := t.resolveUIActor(r)
	if err != nil || actor == "" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	key := chatPreferenceKey(actor)
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		preferences := chatPreferences{}
		if err := t.store.GetSetting(r.Context(), key, &preferences); err != nil && !errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "could not read chat preferences", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, preferences)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, HEAD, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var preferences chatPreferences
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&preferences); err != nil {
		http.Error(w, "invalid chat preferences", http.StatusBadRequest)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		http.Error(w, "invalid chat preferences", http.StatusBadRequest)
		return
	}
	if err := t.store.PutSetting(r.Context(), key, preferences); err != nil {
		http.Error(w, "could not save chat preferences", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, preferences)
}

func (t *Tenant) chatSharePresence(ctx context.Context, actor string) bool {
	if strings.TrimSpace(actor) == "" {
		return false
	}
	var preferences chatPreferences
	if err := t.store.GetSetting(ctx, chatPreferenceKey(actor), &preferences); err != nil {
		return false
	}
	return preferences.SharePresence
}

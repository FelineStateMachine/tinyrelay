package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const sessionCookie = "tiny_session"

// requestURL binds signatures to the original external request, including a
// tenant path stripped by the router. Forwarded headers are never trusted.
func (t *Tenant) requestURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if base, err := url.Parse(t.app.cfg.PublicURL); err == nil && base.Scheme == "https" {
		scheme = "https"
	}
	requestURI := r.RequestURI
	if requestURI == "" {
		requestURI = r.URL.RequestURI()
	}
	if strings.HasPrefix(requestURI, "http://") || strings.HasPrefix(requestURI, "https://") {
		if u, err := url.Parse(requestURI); err == nil {
			requestURI = u.RequestURI()
		}
	}
	return scheme + "://" + r.Host + requestURI
}

func (t *Tenant) initSessions(ctx context.Context) error {
	_, err := t.store.DB().ExecContext(ctx, `CREATE TABLE IF NOT EXISTS browser_sessions(token_hash TEXT PRIMARY KEY,pubkey TEXT NOT NULL,expires_at INTEGER NOT NULL)`)
	return err
}

func sessionHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (t *Tenant) cookieActor(r *http.Request) (string, error) {
	cookie, err := r.Cookie(sessionCookie)
	if errors.Is(err, http.ErrNoCookie) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var actor string
	err = t.store.DB().QueryRowContext(r.Context(), "SELECT pubkey FROM browser_sessions WHERE token_hash=? AND expires_at>?", sessionHash(cookie.Value), time.Now().Unix()).Scan(&actor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	banned, err := t.community.IsBanned(r.Context(), actor)
	if banned {
		return "", errors.New("blocked: this pubkey is banned")
	}
	return actor, err
}

func (t *Tenant) sessionHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	base, err := url.Parse(t.requestURL(r))
	if err != nil {
		http.Error(w, "invalid relay URL", 500)
		return
	}
	// Cookie-only requests may change session state only from this origin.
	if origin := r.Header.Get("Origin"); origin != "" && origin != base.Scheme+"://"+base.Host {
		http.Error(w, "foreign origin", http.StatusForbidden)
		return
	}
	cookiePath := strings.TrimSuffix(base.Path, r.URL.Path) + "/"
	cookie := &http.Cookie{Name: sessionCookie, Path: cookiePath, HttpOnly: true, Secure: base.Scheme == "https", SameSite: http.SameSiteStrictMode}
	if r.URL.Path == "/session/logout" {
		if r.Header.Get("Origin") == "" && r.Header.Get("Authorization") == "" {
			http.Error(w, "origin or signature required", http.StatusForbidden)
			return
		}
		if r.Header.Get("Origin") == "" {
			if _, err := t.resolveUIActor(r); err != nil {
				http.Error(w, "invalid logout signature", http.StatusUnauthorized)
				return
			}
		}
		if prior, err := r.Cookie(sessionCookie); err == nil {
			if _, err := t.store.DB().ExecContext(r.Context(), "DELETE FROM browser_sessions WHERE token_hash=?", sessionHash(prior.Value)); err != nil {
				http.Error(w, "logout failed", 500)
				return
			}
		}
		cookie.MaxAge = -1
		http.SetCookie(w, cookie)
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	body, err := t.requestBody(w, r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	session, err := t.session(r, body)
	if err != nil {
		http.Error(w, err.Error(), 401)
		return
	}
	actor := first(session.PubKeys)
	banned, err := t.community.IsBanned(r.Context(), actor)
	if err != nil {
		http.Error(w, "login failed", 500)
		return
	}
	if banned {
		http.Error(w, "blocked: this pubkey is banned", 403)
		return
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		http.Error(w, "login failed", 500)
		return
	}
	cookie.Value = hex.EncodeToString(token[:])
	cookie.MaxAge = int((24 * time.Hour) / time.Second)
	cookie.Expires = time.Now().Add(24 * time.Hour)
	if _, err := t.store.DB().ExecContext(r.Context(), "INSERT INTO browser_sessions(token_hash,pubkey,expires_at) VALUES(?,?,?)", sessionHash(cookie.Value), actor, cookie.Expires.Unix()); err != nil {
		http.Error(w, "login failed", 500)
		return
	}
	http.SetCookie(w, cookie)
	if actor == t.Policy().Owner {
		if err := t.records.Heartbeat(r.Context(), actor, time.Now().Unix()); err != nil {
			t.app.telemetry.Logger().Error("owner heartbeat", "error", err)
		}
	}
	writeJSON(w, 200, map[string]any{"pubkey": actor})
}

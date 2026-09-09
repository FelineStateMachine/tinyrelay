package daemon

// GET /views/<name>/<hash>[.<svg|png>] serves a custom view artifact under a
// sandbox policy. A members-only view answers only members; a public one
// follows the relay's read rule.

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/views"
)

func isViewArtifactPath(path string) bool {
	return strings.HasPrefix(path, "/views/")
}

func (t *Tenant) viewArtifactHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, finish := t.app.telemetry.Start(r.Context(), "view")
	r = r.WithContext(ctx)
	outcome := "error"
	defer func() { finish(outcome) }()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		outcome = "invalid"
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/views/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		outcome = "invalid"
		return
	}
	name := parts[0]
	hash, extension, hasExtension := strings.Cut(parts[1], ".")
	if !views.NamePattern.MatchString(name) || !views.HashPattern.MatchString(hash) || (hasExtension && extension != "svg" && extension != "png") {
		http.NotFound(w, r)
		outcome = "invalid"
		return
	}
	actor, err := t.resolveUIActor(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		outcome = "unauthorized"
		return
	}
	record, err := t.customViewArtifact(ctx, name, hash)
	if err != nil {
		if strings.HasPrefix(err.Error(), "not found:") {
			http.NotFound(w, r)
			outcome = "invalid"
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	wantType := map[string]string{"svg": "image/svg+xml", "png": "image/png"}[extension]
	if extension != "" && record.Type != wantType {
		http.NotFound(w, r)
		outcome = "invalid"
		return
	}
	if record.Audience == "members" {
		role, err := t.community.Role(ctx, actor)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if actor == "" {
			http.Error(w, "auth-required: this view is for members", http.StatusUnauthorized)
			outcome = "unauthorized"
			return
		}
		if role != "member" && role != "moderator" && role != "owner" {
			http.Error(w, "restricted: this view is for members", http.StatusForbidden)
			outcome = "unauthorized"
			return
		}
	} else if err := t.browseRead(ctx, actor); err != nil {
		browseHTTPError(w, err)
		outcome = "unauthorized"
		return
	}
	body, err := record.body()
	if err != nil {
		http.Error(w, "artifact is unreadable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", record.Type)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	etagBytes := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(etagBytes[:]) + `"`
	w.Header().Set("ETag", etag)
	if record.Audience == "members" {
		w.Header().Set("Cache-Control", "private, no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, no-cache")
	}
	outcome = "ok"
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}

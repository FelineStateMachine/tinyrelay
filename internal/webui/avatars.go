package webui

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/seedmark"
)

var avatarPath = regexp.MustCompile(`^/avatars/v1/([0-9a-f]{64})\.svg$`)

func avatarURL(pubkey string) string {
	key := strings.ToLower(pubkey)
	if !eventIDPattern.MatchString(key) {
		key = strings.Repeat("0", 64)
	}
	return "/avatars/v1/" + key + ".svg"
}

func roomAvatarURL(relay, id string) string {
	seed := sha256.Sum256([]byte("tinyrelay:room:v1\x00" + strings.TrimRight(relay, "/") + "\x00" + id))
	return avatarURL(fmt.Sprintf("%x", seed))
}

// Avatars depend only on the supplied seed and never look up tenant, room or
// profile data. Versioned URLs keep each identity's appearance stable.
func serveAvatar(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	match := avatarPath.FindStringSubmatch(r.URL.Path)
	if match == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", `"seedmark-v1-`+match[1]+`"`)
	http.ServeContent(w, r, "avatar.svg", time.Time{}, strings.NewReader(seedmark.Avatar(match[1])))
}

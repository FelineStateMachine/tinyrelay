package auth

import (
	"errors"
	"net/url"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// ErrGRASP08Unauthorized is the detail-free GRASP-08 authentication error
// returned for every failed repository proof.
var ErrGRASP08Unauthorized = errors.New("auth-required:")

// VerifyGRASP08 validates a reusable kind 27235 GET proof for the repository
// root named by a GRASP-08 Smart HTTP request. The proof remains valid for
// one minute across the repository's supported Git requests.
func (v *Validator) VerifyGRASP08(header, rawURL string) (event.Event, error) {
	e, err := decodeToken(header, "GRASP-08")
	if err != nil {
		return event.Event{}, ErrGRASP08Unauthorized
	}
	if err := event.Validate(e); err != nil || e.Kind != 27235 {
		return event.Event{}, ErrGRASP08Unauthorized
	}
	now := v.now()
	if e.CreatedAt < now.Unix()-60 || e.CreatedAt > now.Unix()+60 {
		return event.Event{}, ErrGRASP08Unauthorized
	}
	if event.Tag(e, "method") != "GET" {
		return event.Event{}, ErrGRASP08Unauthorized
	}
	root, ok := GRASP08RepositoryRoot(rawURL)
	if !ok || sameRequestURL(event.Tag(e, "u"), root) != nil {
		return event.Event{}, ErrGRASP08Unauthorized
	}
	return e, nil
}

// GRASP08RepositoryRoot returns the canonical repository URL for a supported
// Smart HTTP request beneath a .git path.
func GRASP08RepositoryRoot(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", false
	}
	// Keep the client's percent-encoding so the root compares equal to the
	// signed u tag, which is also kept as sent.
	path := u.EscapedPath()
	i := strings.LastIndex(path, ".git")
	if i < 0 || (i+4 < len(path) && path[i+4] != '/') {
		return "", false
	}
	rootPath := path[:i+4]
	suffix := path[i+4:]
	if suffix != "" && suffix != "/info/refs" && suffix != "/git-upload-pack" && suffix != "/git-receive-pack" {
		return "", false
	}
	if strings.HasSuffix(rootPath, "/") {
		return "", false
	}
	unescaped, err := url.PathUnescape(rootPath)
	if err != nil {
		return "", false
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment = unescaped, rootPath, "", ""
	return u.String(), true
}

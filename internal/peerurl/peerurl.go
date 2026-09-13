// Package peerurl matches configured private peers across HTTP and WebSocket transports.
package peerurl

import (
	"net/url"
	"strings"
)

// Match returns the normalized configured base for a source URL.
// WebSocket schemes are mapped to their HTTP equivalents because private Git
// transport probes and signs HTTP repository roots.
func Match(source string, peers []string) string {
	u, err := url.Parse(source)
	if err != nil {
		return ""
	}
	for _, configured := range peers {
		peer, err := url.Parse(strings.TrimRight(strings.TrimSpace(configured), "/"))
		if err != nil || normalizePeerScheme(peer.Scheme) != normalizePeerScheme(u.Scheme) || peer.Host != u.Host {
			continue
		}
		base := strings.TrimRight(peer.Path, "/")
		if base != "" && u.Path != base && !strings.HasPrefix(u.Path, base+"/") {
			continue
		}
		peer.Scheme = normalizePeerScheme(peer.Scheme)
		peer.Path = base
		peer.RawPath = ""
		return strings.TrimRight(peer.String(), "/")
	}
	return ""
}

func normalizePeerScheme(scheme string) string {
	switch scheme {
	case "ws":
		return "http"
	case "wss":
		return "https"
	default:
		return scheme
	}
}

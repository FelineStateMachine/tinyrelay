package tinyclient

import (
	"io"
	"net/http"
	"strings"
)

var immutablePNGAssets = map[string][]byte{
	"/icon-192.png":          icon192PNG,
	"/icon-512.png":          icon512PNG,
	"/icon-maskable-512.png": iconMaskablePNG,
	"/apple-touch-icon.png":  appleTouchIconPNG,
	"/badge-96.png":          badgePNG,
	"/screenshot-narrow.png": screenshotNarrowPNG,
	"/screenshot-wide.png":   screenshotWidePNG,
}

// serveEmbeddedAsset serves assets that do not depend on the backend
// snapshot. It returns false for unknown paths or unsupported methods.
func serveEmbeddedAsset(w http.ResponseWriter, r *http.Request, path string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if strings.HasPrefix(path, "/scripts/") {
		source, ok := scripts[strings.TrimPrefix(path, "/scripts/")]
		if !ok {
			http.NotFound(w, r)
			return true
		}
		w.Header().Set("content-type", "application/javascript; charset=utf-8")
		if r.URL.Query().Get("v") == scriptsVersion {
			w.Header().Set("cache-control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("cache-control", "no-cache")
		}
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, source)
		}
		return true
	}
	if path == "/signer.js" {
		return writeEmbeddedAsset(w, r, "application/javascript; charset=utf-8", signerJS, "")
	}
	if path == "/fixi.js" {
		return writeEmbeddedAsset(w, r, "application/javascript; charset=utf-8", fixiJS, "")
	}
	if path == "/sw.js" {
		w.Header().Set("service-worker-allowed", "/")
		return writeEmbeddedAsset(w, r, "application/javascript; charset=utf-8", serviceWorkerJS, "")
	}
	if path == "/icon.svg" {
		return writeEmbeddedAsset(w, r, "image/svg+xml; charset=utf-8", iconSVG, "")
	}
	if path == "/icon-mono.svg" {
		return writeEmbeddedAsset(w, r, "image/svg+xml; charset=utf-8", iconMonoSVG, "")
	}
	if png, ok := immutablePNGAssets[path]; ok {
		return writeEmbeddedAsset(w, r, "image/png", png, "public, max-age=86400")
	}
	return false
}

func writeEmbeddedAsset(w http.ResponseWriter, r *http.Request, contentType string, body []byte, cacheControl string) bool {
	w.Header().Set("content-type", contentType)
	if cacheControl != "" {
		w.Header().Set("cache-control", cacheControl)
	}
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
	return true
}

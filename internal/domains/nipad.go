package domains

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

// AddressLookup returns the Nostr filter for a validated web path. The
// daemon supplies this from its event/site index; the domains package keeps
// the discovery wire contract independent of storage.
type AddressLookup func(context.Context, string) (map[string]any, bool, error)

type AddressConfig struct {
	RelayURL    string
	Authorized  func(*http.Request) bool
	ReadAllowed func(*http.Request) bool
	Lookup      AddressLookup
}

func WebAddressHandler(cfg AddressConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveWebAddress(w, r, cfg) })
}

func serveWebAddress(w http.ResponseWriter, r *http.Request, cfg AddressConfig) {
	setAddressHeaders(w, r.Method)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	path, ok := requestedPath(r.URL)
	if !ok {
		writeAddress(w, r, http.StatusBadRequest, map[string]any{})
		return
	}
	if cfg.Authorized != nil && !cfg.Authorized(r) {
		writeAddress(w, r, http.StatusUnauthorized, map[string]any{})
		return
	}
	if cfg.ReadAllowed != nil && !cfg.ReadAllowed(r) {
		writeAddress(w, r, http.StatusForbidden, map[string]any{})
		return
	}
	result := map[string]any{}
	if cfg.Lookup != nil {
		filter, found, err := cfg.Lookup(r.Context(), path)
		if err != nil {
			http.Error(w, "lookup failed", http.StatusInternalServerError)
			return
		}
		if found {
			result[path] = map[string]any{"filter": filter, "relays": []string{strings.TrimRight(cfg.RelayURL, "/")}}
		}
	}
	writeAddress(w, r, http.StatusOK, result)
}

func requestedPath(raw *url.URL) (string, bool) {
	values, ok := raw.Query()["path"]
	if !ok || len(values) != 1 {
		return "", false
	}
	path := values[0]
	if len(path) > 4096 || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return "", false
	}
	if strings.ContainsAny(path, "\\?#\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x7f") {
		return "", false
	}
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return "", false
	}
	if strings.HasPrefix(decoded, "//") || strings.ContainsAny(decoded, "\\\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x7f") {
		return "", false
	}
	for _, segment := range strings.Split(decoded, "/") {
		if segment == "." || segment == ".." {
			return "", false
		}
		for _, r := range segment {
			if unicode.IsSpace(r) {
				return "", false
			}
		}
	}
	return path, true
}

func setAddressHeaders(w http.ResponseWriter, method string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "authorization, accept")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = method
}

func writeAddress(w http.ResponseWriter, r *http.Request, status int, value map[string]any) {
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(w).Encode(value)
}

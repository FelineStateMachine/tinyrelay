package webui

import (
	"net/url"
	"strings"
)

// signinURL returns the sign-in route, carrying the current tenant-local
// address so the signer can return the viewer to the page they opened.
// Fragments are intentionally absent here because browsers do not send them
// in HTTP requests; client-side navigation may preserve one when present.
func signinURL(base, path string, query url.Values) string {
	path = "/" + strings.TrimPrefix(path, "/")
	if path == "/signin" {
		return path
	}
	if base != "" && base != "/" {
		path = joinTenantPath(base, path)
	}
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return "/signin?next=" + url.QueryEscape(path)
}

func joinTenantPath(base, path string) string {
	base = strings.TrimSuffix("/"+strings.Trim(base, "/"), "/")
	if base == "/" {
		return "/" + strings.TrimPrefix(path, "/")
	}
	return base + "/" + strings.TrimPrefix(path, "/")
}

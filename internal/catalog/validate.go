package catalog

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

var labelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 128 || strings.ContainsAny(name, "/\\") || hasControl(name) {
		return "", errors.New("tenant name must be non-empty, safe, and at most 128 characters")
	}
	return name, nil
}

func normalizeOwner(owner string) (string, error) {
	if len(owner) != 64 || owner != strings.ToLower(owner) {
		return "", errors.New("owner must be a lowercase 32-byte hex public key")
	}
	decoded, err := hex.DecodeString(owner)
	if err != nil || len(decoded) != 32 {
		return "", errors.New("owner must be a lowercase 32-byte hex public key")
	}
	return owner, nil
}

func normalizeTemplate(template string) (string, error) {
	template = strings.TrimSpace(template)
	if template == "" {
		return "default", nil
	}
	if len(template) > 64 || strings.ContainsAny(template, "/\\") || hasControl(template) {
		return "", errors.New("tenant template must be safe and at most 64 characters")
	}
	return template, nil
}

func normalizeSource(source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", nil
	}
	u, err := url.Parse(source)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "ws" && u.Scheme != "wss") || u.User != nil {
		return "", errors.New("source must be an http, https, ws, or wss relay URL")
	}
	return source, nil
}

func normalizeHost(host string) (string, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "/\\:@?#") || hasControl(host) {
		return "", errors.New("hostname must be a normalized DNS name")
	}
	if net.ParseIP(host) != nil {
		return host, nil
	}
	for _, label := range strings.Split(host, ".") {
		if !labelPattern.MatchString(label) {
			return "", fmt.Errorf("invalid hostname label %q", label)
		}
	}
	return host, nil
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func isConstraint(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "constraint") || errors.Is(err, sql.ErrNoRows)
}

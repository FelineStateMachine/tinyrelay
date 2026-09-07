// Package sites implements NIP-5A manifests and the self-hosted site door.
package sites

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

const (
	KindSite         = 15128
	KindNamedSite    = 35128
	KindSiteSnapshot = 5128
)

var (
	hexPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	namePattern = regexp.MustCompile(`^[a-z0-9-]{1,13}$`)
	pathPattern = regexp.MustCompile(`^/(?:[^/]+/)*[^/.]+\.[^/.]+$`)
)

type Site struct {
	Kind   int
	PubKey string
	D      string
	ID     string
}

func Base36(value string) (string, error) {
	if !hexPattern.MatchString(value) {
		return "", errors.New("expected lowercase 32-byte hex")
	}
	n := new(big.Int)
	if _, ok := n.SetString(value, 16); !ok {
		return "", errors.New("invalid hexadecimal value")
	}
	value36 := n.Text(36)
	return strings.Repeat("0", 50-len(value36)) + value36, nil
}

func Unbase36(value string) (string, bool) {
	if len(value) != 50 || !regexp.MustCompile(`^[0-9a-z]+$`).MatchString(value) {
		return "", false
	}
	n, ok := new(big.Int).SetString(value, 36)
	if !ok || n.BitLen() > 256 {
		return "", false
	}
	return fmt.Sprintf("%064x", n), true
}

func ParseSite(label string) (Site, bool) {
	if strings.HasPrefix(label, "npub1") {
		key, ok := decodeNpub(label)
		if ok {
			return Site{Kind: KindSite, PubKey: key.Hex()}, true
		}
	}
	if strings.HasPrefix(label, "v") && len(label) == 51 {
		id, ok := Unbase36(label[1:])
		if ok {
			return Site{Kind: KindSiteSnapshot, ID: id}, true
		}
	}
	if len(label) < 51 || len(label) > 63 || strings.HasSuffix(label, "-") {
		return Site{}, false
	}
	key, ok := Unbase36(label[:50])
	if !ok || !namePattern.MatchString(label[50:]) {
		return Site{}, false
	}
	return Site{Kind: KindNamedSite, PubKey: key, D: label[50:]}, true
}

func SiteLabel(e event.Event) string {
	switch e.Kind {
	case KindSite:
		key, err := nostr.PubKeyFromHex(e.PubKey)
		if err != nil {
			return ""
		}
		return encodeNpub(key)
	case KindNamedSite:
		key, err := Base36(e.PubKey)
		if err != nil {
			return ""
		}
		return key + event.Tag(e, "d")
	case KindSiteSnapshot:
		key, err := Base36(e.ID)
		if err != nil {
			return ""
		}
		return "v" + key
	default:
		return ""
	}
}

func SitePaths(e event.Event) [][]string {
	paths := make([][]string, 0)
	for _, tag := range e.Tags {
		if len(tag) > 0 && tag[0] == "path" {
			paths = append(paths, tag)
		}
	}
	return paths
}

func Aggregate(paths [][]string) string {
	lines := make([]string, 0, len(paths))
	for _, tag := range paths {
		if len(tag) == 3 && tag[0] == "path" {
			lines = append(lines, tag[2]+" "+tag[1]+"\n")
		}
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "")))
	return hex.EncodeToString(sum[:])
}

func ValidateManifest(e event.Event) error {
	if e.Kind != KindSite && e.Kind != KindNamedSite && e.Kind != KindSiteSnapshot {
		return nil
	}
	dTags := tagsNamed(e, "d")
	if (e.Kind == KindNamedSite && (len(dTags) != 1 || !namePattern.MatchString(dTags[0][1]))) || (e.Kind != KindNamedSite && len(dTags) != 0) {
		return errors.New("invalid: site d tag")
	}
	paths := SitePaths(e)
	if len(paths) == 0 {
		return errors.New("invalid: site needs a path")
	}
	seen := map[string]bool{}
	for _, tag := range paths {
		if len(tag) != 3 || !pathPattern.MatchString(tag[1]) || strings.ContainsAny(tag[1], "\\?#\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x7f") || hasDotSegment(tag[1]) || !hexPattern.MatchString(tag[2]) {
			return errors.New("invalid: site path")
		}
		if seen[tag[1]] {
			return errors.New("invalid: duplicate site path")
		}
		seen[tag[1]] = true
	}
	if err := validateAggregate(e); err != nil {
		return err
	}
	return validateLineage(e)
}

func validateAggregate(e event.Event) error {
	x := tagsNamed(e, "x")
	if len(x) > 1 || (e.Kind == KindSiteSnapshot && len(x) != 1) {
		return errors.New("invalid: site x tag")
	}
	if len(x) == 1 && (len(x[0]) != 3 || x[0][2] != "aggregate" || x[0][1] != Aggregate(SitePaths(e))) {
		return errors.New("invalid: site aggregate hash")
	}
	return nil
}

func validateLineage(e event.Event) error {
	a, A := tagsNamed(e, "a"), tagsNamed(e, "A")
	for _, tag := range append(append([][]string{}, a...), A...) {
		if len(tag) != 2 || !validReference(tag[1]) {
			return errors.New("invalid: site lineage")
		}
	}
	if e.Kind == KindSiteSnapshot && len(a) != 1 {
		return errors.New("invalid: snapshot source")
	}
	if e.Kind != KindSiteSnapshot && len(a) != len(A) {
		return errors.New("invalid: copied site lineage")
	}
	return nil
}

func validReference(value string) bool {
	parts := strings.Split(value, ":")
	if len(parts) != 3 || !hexPattern.MatchString(parts[1]) || parts[0] != fmt.Sprint(KindSite) && parts[0] != fmt.Sprint(KindNamedSite) {
		return false
	}
	return parts[0] == fmt.Sprint(KindSite) && parts[2] == "" || parts[0] == fmt.Sprint(KindNamedSite) && namePattern.MatchString(parts[2])
}

func tagsNamed(e event.Event, name string) [][]string {
	out := make([][]string, 0)
	for _, tag := range e.Tags {
		if len(tag) > 0 && tag[0] == name {
			out = append(out, tag)
		}
	}
	return out
}
func hasDotSegment(path string) bool {
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return true
		}
	}
	return false
}

func SiteType(path string) string {
	ext := strings.ToLower(strings.TrimPrefix(path, "."))
	i := strings.LastIndexByte(ext, '.')
	if i >= 0 {
		ext = ext[i+1:]
	}
	if typ := map[string]string{"html": "text/html; charset=utf-8", "htm": "text/html; charset=utf-8", "css": "text/css", "js": "text/javascript", "mjs": "text/javascript", "json": "application/json", "svg": "image/svg+xml", "png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp", "ico": "image/x-icon", "txt": "text/plain; charset=utf-8", "xml": "application/xml", "wasm": "application/wasm", "woff": "font/woff", "woff2": "font/woff2", "pdf": "application/pdf"}[ext]; typ != "" {
		return typ
	}
	return "application/octet-stream"
}

func RequestedPath(raw string) (string, bool) {
	decoded, err := url.PathUnescape(raw)
	return decoded, err == nil
}

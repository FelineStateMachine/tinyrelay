package blob

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// BlossomURI is the portable BUD-10 representation of a blob reference.
type BlossomURI struct {
	Hash      string
	Extension string
	Servers   []string
	Author    string
	Authors   []string
	Size      int64
}

var hashInURL = regexp.MustCompile(`(?i)([0-9a-f]{64})(?:\.[a-z0-9]{1,16})?(?:$|[^0-9a-f])`)

// ParseBlossomURI parses blossom:<sha256>[.extension] and its optional
// server, author, and size hints.
func ParseBlossomURI(raw string) (BlossomURI, error) {
	if !strings.HasPrefix(strings.ToLower(raw), "blossom:") {
		return BlossomURI{}, errors.New("invalid: Blossom URI scheme")
	}
	body := raw[len("blossom:"):]
	parts := strings.SplitN(body, "?", 2)
	path := parts[0]
	base := path
	if dot := strings.IndexByte(path, '.'); dot >= 0 {
		base = path[:dot]
	}
	if !shaPattern.MatchString(strings.ToLower(base)) {
		return BlossomURI{}, errors.New("invalid: Blossom URI hash")
	}
	result := BlossomURI{Hash: strings.ToLower(base)}
	if len(path) > len(base) {
		result.Extension = strings.ToLower(path[len(base)+1:])
		if !regexp.MustCompile(`^[a-z0-9]{1,16}$`).MatchString(result.Extension) {
			return BlossomURI{}, errors.New("invalid: Blossom URI extension")
		}
	} else {
		result.Extension = "bin"
	}
	if len(parts) == 1 {
		return result, nil
	}
	values, err := url.ParseQuery(parts[1])
	if err != nil {
		return BlossomURI{}, fmt.Errorf("invalid: Blossom URI query: %w", err)
	}
	for _, server := range values["xs"] {
		if !validServerHint(server) {
			return BlossomURI{}, errors.New("invalid: Blossom server hint")
		}
		result.Servers = append(result.Servers, strings.TrimRight(strings.TrimSpace(server), "/"))
	}
	for _, author := range values["as"] {
		if !shaPattern.MatchString(strings.ToLower(author)) {
			return BlossomURI{}, errors.New("invalid: Blossom author hint")
		}
		result.Authors = append(result.Authors, strings.ToLower(author))
	}
	if len(result.Authors) > 0 {
		result.Author = result.Authors[0]
	}
	if rawSize := values.Get("sz"); rawSize != "" {
		result.Size, err = strconv.ParseInt(rawSize, 10, 64)
		if err != nil || result.Size <= 0 {
			return BlossomURI{}, errors.New("invalid: Blossom URI size")
		}
	}
	return result, nil
}

func validServerHint(raw string) bool {
	value := strings.TrimSpace(raw)
	if value == "" || strings.ContainsAny(value, " \t\r\n@") {
		return false
	}
	parsed, err := url.Parse(normalizeServerHint(value))
	return err == nil && parsed.Hostname() != "" && parsed.User == nil && parsed.Path == ""
}

func normalizeServerHint(raw string) string {
	value := strings.TrimRight(strings.TrimSpace(raw), "/")
	if !strings.Contains(value, "://") {
		return "https://" + value
	}
	return value
}

// String returns a canonical BUD-10 URI with stable query ordering.
func (u BlossomURI) String() string {
	path := "blossom:" + u.Hash
	if u.Extension != "" {
		path += "." + u.Extension
	}
	values := url.Values{}
	for _, server := range u.Servers {
		values.Add("xs", strings.TrimPrefix(strings.TrimPrefix(server, "https://"), "http://"))
	}
	if len(u.Authors) > 0 || u.Author != "" {
		if len(u.Authors) == 0 {
			values.Set("as", u.Author)
		} else {
			for _, author := range u.Authors {
				values.Add("as", author)
			}
		}
	}
	if u.Size > 0 {
		values.Set("sz", strconv.FormatInt(u.Size, 10))
	}
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return path
}

// BlossomServerList extracts the ordered BUD-03 server list from kind 10063.
func BlossomServerList(e event.Event) ([]string, error) {
	if e.Kind != 10063 {
		return nil, errors.New("invalid: event is not a Blossom server list")
	}
	servers := make([]string, 0)
	seen := make(map[string]struct{})
	for _, tag := range e.Tags {
		if len(tag) < 2 || tag[0] != "server" {
			continue
		}
		parsed, err := url.Parse(strings.TrimSpace(tag[1]))
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return nil, errors.New("invalid: Blossom server list URL")
		}
		server := strings.TrimRight(parsed.String(), "/")
		if _, ok := seen[server]; ok {
			continue
		}
		seen[server] = struct{}{}
		servers = append(servers, server)
	}
	if len(servers) == 0 {
		return nil, errors.New("invalid: Blossom server list is empty")
	}
	return servers, nil
}

// BlobHashFromURL returns the last 64-hex component in a URL, as required by
// BUD-03 and NIP-B7 for recovery from a broken original URL.
func BlobHashFromURL(raw string) string {
	matches := hashInURL.FindAllStringSubmatch(raw, -1)
	if len(matches) == 0 {
		return ""
	}
	return strings.ToLower(matches[len(matches)-1][1])
}

// FallbackURLs produces ordered server URLs for a recovered blob.
func FallbackURLs(servers []string, hash, extension string) []string {
	if !shaPattern.MatchString(strings.ToLower(hash)) {
		return nil
	}
	if extension != "" && !strings.HasPrefix(extension, ".") {
		extension = "." + extension
	}
	result := make([]string, 0, len(servers))
	for _, server := range servers {
		raw := strings.TrimRight(strings.TrimSpace(server), "/")
		if strings.Contains(raw, "://") {
			result = append(result, raw+"/"+strings.ToLower(hash)+extension)
		} else {
			result = append(result, "https://"+raw+"/"+strings.ToLower(hash)+extension)
			result = append(result, "http://"+raw+"/"+strings.ToLower(hash)+extension)
		}
	}
	return result
}

// SortedServerList is useful for operator display without changing the
// original ordered list used for client failover.
func SortedServerList(servers []string) []string {
	result := append([]string(nil), servers...)
	sort.Strings(result)
	return result
}

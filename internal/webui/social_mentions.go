package webui

import (
	"encoding/hex"
	"html"
	"net/url"
	"regexp"
	"strings"
)

var socialNostrAnchor = regexp.MustCompile(`<a href="/open\?target=([^"&]+)">((?:nostr:|web\+nostr:)[^<]+)</a>`)

// socialNostrLinks gives NIP-27 references useful labels after autolinking.
// It only accepts the exact anchors emitted by autolinkOutsideTags.
func socialNostrLinks(rendered string) string {
	return socialNostrAnchor.ReplaceAllStringFunc(rendered, func(anchor string) string {
		match := socialNostrAnchor.FindStringSubmatch(anchor)
		target, err := url.QueryUnescape(html.UnescapeString(match[1]))
		if err != nil || !strings.EqualFold(target, html.UnescapeString(match[2])) {
			return anchor
		}
		path, label, ok := socialNostrDestination(target)
		if !ok {
			return anchor
		}
		return `<a href="` + html.EscapeString(path) + `">` + label + `</a>`
	})
}

func socialNostrDestination(target string) (string, string, bool) {
	value := strings.Trim(strings.TrimSpace(target), "/")
	for _, prefix := range []string{"web+nostr:", "nostr:"} {
		if strings.HasPrefix(strings.ToLower(value), prefix) {
			value = value[len(prefix):]
			break
		}
	}
	hrp, data, ok := bech32Decode(strings.ToLower(value))
	if !ok {
		return "", "", false
	}
	switch hrp {
	case "npub", "nprofile":
		fields := data
		if hrp == "nprofile" {
			fields = tlvFields(data)[0]
		}
		if len(fields) != 32 {
			return "", "", false
		}
		pubkey := hex.EncodeToString(fields)
		return "/social?author=" + pubkey, `<nostr-name pubkey="` + pubkey + `">` + shortID(pubkey) + `</nostr-name>`, true
	case "note", "nevent":
		id := data
		if hrp == "nevent" {
			id = tlvFields(data)[0]
		}
		if len(id) != 32 {
			return "", "", false
		}
		return "/e/" + hex.EncodeToString(id), "Quoted post", true
	case "naddr":
		path, ok := nostrTargetPath(target)
		label := "View post"
		if strings.HasPrefix(path, "/a/30023:") {
			label = "Article"
		}
		return path, label, ok
	default:
		return "", "", false
	}
}

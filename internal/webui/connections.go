package webui

import (
	"encoding/binary"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
)

// bech32 encodes data with the given human readable part.
func bech32(hrp string, data []byte) string {
	fiveBits := convertBits(data)
	expanded := make([]byte, 0, len(hrp)*2+1)
	for _, c := range hrp {
		expanded = append(expanded, byte(c)>>5)
	}
	expanded = append(expanded, 0)
	for _, c := range hrp {
		expanded = append(expanded, byte(c)&31)
	}
	values := append(append(append([]byte{}, expanded...), fiveBits...), 0, 0, 0, 0, 0, 0)
	polymod := bech32Polymod(values) ^ 1
	checksum := make([]byte, 6)
	for i := range checksum {
		checksum[i] = byte(polymod >> uint(5*(5-i)) & 31)
	}
	var out strings.Builder
	out.WriteString(hrp + "1")
	for _, value := range append(fiveBits, checksum...) {
		out.WriteByte(bech32Charset[value])
	}
	return out.String()
}

func tlv(kind byte, value []byte) []byte {
	return append([]byte{kind, byte(len(value))}, value...)
}

// nprofile encodes a NIP-19 profile pointer with one relay hint.
func nprofile(pubkeyHex, relay string) string {
	pubkey, err := hex.DecodeString(pubkeyHex)
	if err != nil || len(pubkey) != 32 {
		return ""
	}
	data := tlv(0, pubkey)
	if relay != "" {
		data = append(data, tlv(1, []byte(relay))...)
	}
	return bech32("nprofile", data)
}

// naddr encodes a NIP-19 address pointer for a replaceable event.
func naddr(kind uint32, pubkeyHex, identifier, relay string) string {
	pubkey, err := hex.DecodeString(pubkeyHex)
	if err != nil || len(pubkey) != 32 {
		return ""
	}
	data := tlv(0, []byte(identifier))
	if relay != "" {
		data = append(data, tlv(1, []byte(relay))...)
	}
	data = append(data, tlv(2, pubkey)...)
	kindBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(kindBytes, kind)
	data = append(data, tlv(3, kindBytes)...)
	return bech32("naddr", data)
}

var placeholder = regexp.MustCompile(`\{([a-z]+):([a-z]+)(?:\|(enc))?\}`)

// connectionValues resolves the placeholders connection templates use:
// relay:url, relay:web, relay:host, relay:domain, relay:naddr, owner:npub,
// owner:nprofile, user:npub and user:nprofile. The relay's group address is
// its NIP-29 top-level group, published under the relay identity.
func connectionValues(data PageData) map[string]string {
	values := map[string]string{}
	web := strings.TrimRight(data.URL, "/")
	values["relay:web"] = web
	values["relay:url"] = wsURL(web)
	if parsed, err := url.Parse(web); err == nil {
		values["relay:host"] = parsed.Host
		values["relay:domain"] = parsed.Hostname()
	}
	if data.Identity != "" {
		values["relay:naddr"] = naddr(39000, data.Identity, "_", values["relay:url"])
	}
	if data.Policy.Owner != "" {
		values["owner:npub"] = identityNpub(data.Policy.Owner)
		values["owner:nprofile"] = nprofile(data.Policy.Owner, values["relay:url"])
	}
	if data.Actor != "" {
		values["user:npub"] = identityNpub(data.Actor)
		values["user:nprofile"] = nprofile(data.Actor, values["relay:url"])
	}
	return values
}

// fillPlaceholders substitutes known values, leaving input placeholders for
// the page script. It reports false when a needed value is missing, such as
// a user value for a guest, so the link can be left out.
func fillPlaceholders(text string, values map[string]string) (string, bool) {
	complete := true
	out := placeholder.ReplaceAllStringFunc(text, func(match string) string {
		parts := placeholder.FindStringSubmatch(match)
		if parts[1] == "input" {
			return match
		}
		value, ok := values[parts[1]+":"+parts[2]]
		if !ok || value == "" {
			complete = false
			return ""
		}
		if parts[3] == "enc" {
			return url.QueryEscape(value)
		}
		return value
	})
	return out, complete
}

// expandConnections turns the daemon's connection rows into ready-to-render
// maps with placeholders resolved. Links that need a signed-in user are
// dropped for guests; the card keeps a note that signing in reveals them.
func expandConnections(rows []any, data PageData) []any {
	values := connectionValues(data)
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		item := valueMap(row)
		card := map[string]any{"name": plainString(item["template"]), "title": plainString(item["title"]), "about": plainString(item["about"]), "app": plainString(item["app"]), "where": plainString(item["where"]), "visibility": plainString(item["visibility"]), "inputs": item["inputs"]}
		if card["title"] == "" {
			card["title"] = card["name"]
		}
		if qr, ok := fillPlaceholders(plainString(item["qr"]), values); ok && qr != "" {
			card["qr"] = qr
		}
		links := []any{}
		hidden := 0
		for _, link := range browseRows(item["links"]) {
			l := valueMap(link)
			field := func(name string) string {
				if value := plainString(l[name]); value != "" {
					return value
				}
				return plainString(l[strings.ToUpper(name[:1])+name[1:]])
			}
			href, okHref := fillPlaceholders(field("href"), values)
			copyText, okCopy := fillPlaceholders(field("copy"), values)
			if !okHref || !okCopy {
				hidden++
				continue
			}
			links = append(links, map[string]any{"label": field("label"), "href": href, "copy": copyText})
		}
		card["links"], card["hidden"] = links, hidden
		out = append(out, card)
	}
	return out
}

package webui

import (
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var hexID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// openLink handles web+nostr links registered through the installed app.
// It decodes the NIP-19 target and redirects to the page that shows it.
func (a *App) openLink(writer http.ResponseWriter, request *http.Request) {
	path, ok := nostrTargetPath(request.URL.Query().Get("target"))
	if !ok {
		http.Error(writer, "unsupported nostr link", http.StatusBadRequest)
		return
	}
	http.Redirect(writer, request, requestPrefix(request)+path, http.StatusSeeOther)
}

// nostrTargetPath maps a nostr: or web+nostr: target to a relay page.
func nostrTargetPath(target string) (string, bool) {
	value := strings.TrimSpace(target)
	for _, prefix := range []string{"web+nostr:", "nostr:"} {
		if strings.HasPrefix(strings.ToLower(value), prefix) {
			value = value[len(prefix):]
			break
		}
	}
	value = strings.Trim(value, "/")
	if hexID.MatchString(value) {
		return "/e/" + value, true
	}
	hrp, data, ok := bech32Decode(strings.ToLower(value))
	if !ok {
		return "", false
	}
	switch hrp {
	case "npub":
		if len(data) != 32 {
			return "", false
		}
		return "/search?author=" + hex.EncodeToString(data), true
	case "note":
		if len(data) != 32 {
			return "", false
		}
		return "/e/" + hex.EncodeToString(data), true
	case "nprofile", "nevent", "naddr":
		fields := tlvFields(data)
		switch hrp {
		case "nprofile":
			if len(fields[0]) != 32 {
				return "", false
			}
			return "/search?author=" + hex.EncodeToString(fields[0]), true
		case "nevent":
			if len(fields[0]) != 32 {
				return "", false
			}
			return "/e/" + hex.EncodeToString(fields[0]), true
		default:
			if len(fields[2]) != 32 || len(fields[3]) != 4 {
				return "", false
			}
			kind := binary.BigEndian.Uint32(fields[3])
			return "/a/" + strconv.FormatUint(uint64(kind), 10) + ":" + hex.EncodeToString(fields[2]) + ":" + url.PathEscape(string(fields[0])), true
		}
	}
	return "", false
}

// tlvFields returns the first value of each NIP-19 TLV type.
func tlvFields(data []byte) map[byte][]byte {
	fields := map[byte][]byte{}
	for len(data) >= 2 {
		kind, length := data[0], int(data[1])
		if len(data) < 2+length {
			break
		}
		if _, seen := fields[kind]; !seen {
			fields[kind] = data[2 : 2+length]
		}
		data = data[2+length:]
	}
	return fields
}

// bech32Decode verifies a bech32 string and returns its 8-bit payload.
func bech32Decode(value string) (string, []byte, bool) {
	separator := strings.LastIndexByte(value, '1')
	if separator < 1 || separator+7 > len(value) || len(value) > 5000 {
		return "", nil, false
	}
	hrp, encoded := value[:separator], value[separator+1:]
	values := make([]byte, len(encoded))
	for i, char := range encoded {
		position := strings.IndexRune(bech32Charset, char)
		if position < 0 {
			return "", nil, false
		}
		values[i] = byte(position)
	}
	if bech32Polymod(append(bech32Expand(hrp), values...)) != 1 {
		return "", nil, false
	}
	data := values[:len(values)-6]
	accumulator, bits := 0, 0
	var result []byte
	for _, v := range data {
		accumulator = accumulator<<5 | int(v)
		bits += 5
		if bits >= 8 {
			bits -= 8
			result = append(result, byte(accumulator>>bits&255))
		}
	}
	if bits >= 5 || accumulator<<(8-bits)&255 != 0 {
		return "", nil, false
	}
	return hrp, result, true
}

func bech32Expand(hrp string) []byte {
	expanded := make([]byte, 0, len(hrp)*2+1)
	for _, c := range hrp {
		expanded = append(expanded, byte(c)>>5)
	}
	expanded = append(expanded, 0)
	for _, c := range hrp {
		expanded = append(expanded, byte(c)&31)
	}
	return expanded
}

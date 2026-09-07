package sites

import (
	"strings"

	"fiatjaf.com/nostr"
)

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func encodeNpub(key nostr.PubKey) string {
	data, _ := convertBits(key[:], 8, 5, true)
	return bech32Encode("npub", data)
}

func decodeNpub(value string) (nostr.PubKey, bool) {
	prefix, data, ok := bech32Decode(value)
	if !ok || prefix != "npub" {
		return nostr.PubKey{}, false
	}
	raw, ok := convertBits(data, 5, 8, false)
	if !ok || len(raw) != 32 {
		return nostr.PubKey{}, false
	}
	var key nostr.PubKey
	copy(key[:], raw)
	return key, true
}

func bech32Encode(prefix string, data []byte) string {
	values := make([]byte, len(data))
	for i, value := range data {
		values[i] = value
	}
	checksum := bech32Checksum(prefix, values)
	all := append(values, checksum...)
	var out strings.Builder
	out.WriteString(prefix)
	out.WriteByte('1')
	for _, value := range all {
		out.WriteByte(bech32Charset[value])
	}
	return out.String()
}

func bech32Decode(value string) (string, []byte, bool) {
	if value != strings.ToLower(value) || len(value) < 8 {
		return "", nil, false
	}
	sep := strings.LastIndexByte(value, '1')
	if sep < 1 || sep+7 > len(value) {
		return "", nil, false
	}
	prefix, encoded := value[:sep], value[sep+1:]
	values := make([]byte, len(encoded))
	for i, char := range encoded {
		pos := strings.IndexByte(bech32Charset, byte(char))
		if pos < 0 {
			return "", nil, false
		}
		values[i] = byte(pos)
	}
	if !bech32Verify(prefix, values) {
		return "", nil, false
	}
	return prefix, values[:len(values)-6], true
}

func convertBits(data []byte, from, to byte, pad bool) ([]byte, bool) {
	var acc uint
	bits := byte(0)
	max := uint((1 << to) - 1)
	out := make([]byte, 0, len(data)*int(from)/int(to)+1)
	for _, value := range data {
		acc = (acc << from) | uint(value)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte((acc>>bits)&max))
		}
	}
	if pad && bits > 0 {
		out = append(out, byte((acc<<uint(to-bits))&max))
	}
	if !pad && (bits >= from || ((acc<<uint(to-bits))&max) != 0) {
		return nil, false
	}
	return out, true
}

func bech32Checksum(prefix string, values []byte) []byte {
	all := append(bech32Expand(prefix), values...)
	all = append(all, 0, 0, 0, 0, 0, 0)
	polymod := bech32Polymod(all) ^ 1
	out := make([]byte, 6)
	for i := range out {
		out[i] = byte((polymod >> uint(5*(5-i))) & 31)
	}
	return out
}

func bech32Verify(prefix string, values []byte) bool {
	return bech32Polymod(append(bech32Expand(prefix), values...)) == 1
}
func bech32Expand(prefix string) []byte {
	out := make([]byte, 0, len(prefix)*2+1)
	for _, c := range prefix {
		out = append(out, byte(c)>>5)
	}
	out = append(out, 0)
	for _, c := range prefix {
		out = append(out, byte(c)&31)
	}
	return out
}
func bech32Polymod(values []byte) uint32 {
	generators := [...]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	var chk uint32 = 1
	for _, value := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(value)
		for i, generator := range generators {
			if top>>uint(i)&1 != 0 {
				chk ^= generator
			}
		}
	}
	return chk
}

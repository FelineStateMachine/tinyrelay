package gates

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
)

var marmotHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
var marmotID = regexp.MustCompile(`^0x[0-9a-f]{4}$`)
var marmotVersion = regexp.MustCompile(`^1\.0$`)
var marmotHex = regexp.MustCompile(`^[0-9a-f]+$`)

// MarmotShape validates relay-visible MLS envelope structure without decoding MLS.
func MarmotShape(e event.Event) error {
	if e.Kind == event.KIND_MARMOT_GROUP {
		return groupShape(e)
	}
	if e.Kind == event.KIND_MARMOT_KEY_PACKAGE {
		return keyPackageShape(e)
	}
	return nil
}

func groupShape(e event.Event) error {
	if !singletonTag(e, "h", marmotHex64) {
		return fmt.Errorf("invalid: kind 445 needs one lowercase 32-byte h tag")
	}
	b, err := base64.StdEncoding.Strict().DecodeString(e.Content)
	if err != nil || len(b) < 28 {
		return fmt.Errorf("invalid: kind 445 content must be padded base64 containing a nonce and authentication tag")
	}
	for _, tag := range e.Tags {
		if len(tag) == 0 || tag[0] != "h" && !(tag[0] == "expiration" && len(tag) == 2 && digits(tag[1])) {
			return fmt.Errorf("invalid: kind 445 has an unsupported tag")
		}
	}
	if countTag(e, "expiration") > 1 {
		return fmt.Errorf("invalid: kind 445 has repeated expiration")
	}
	return nil
}

func keyPackageShape(e event.Event) error {
	if !singletonTag(e, "d", marmotHex64) || !singletonTag(e, "mls_protocol_version", marmotVersion) || !singletonTag(e, "i", marmotHex) {
		return fmt.Errorf("invalid: kind 30443 has a malformed singleton tag")
	}
	for _, name := range []string{"mls_ciphersuite", "mls_extensions", "mls_proposals", "app_components"} {
		if !idList(e, name) {
			return fmt.Errorf("invalid: kind 30443 has a malformed id-list tag")
		}
	}
	for _, tag := range e.Tags {
		if len(tag) > 0 && tag[0] == "app_components" {
			found := false
			for _, id := range tag[1:] {
				if id == "0x8009" {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("invalid: kind 30443 must advertise account identity proof")
			}
		}
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(e.Content)
	if err != nil || len(decoded) == 0 {
		return fmt.Errorf("invalid: kind 30443 content must be padded base64")
	}
	allowed := map[string]bool{"d": true, "mls_protocol_version": true, "i": true, "mls_ciphersuite": true, "mls_extensions": true, "mls_proposals": true, "app_components": true}
	for _, tag := range e.Tags {
		if len(tag) == 0 || !allowed[tag[0]] {
			return fmt.Errorf("invalid: kind 30443 has an unsupported tag")
		}
	}
	return nil
}

// Principal returns the authenticated account whose write policy applies to a group envelope.
func (g *Gate) Principal(ctx context.Context, e event.Event, s relay.Session) (string, error) {
	if e.Kind != event.KIND_MARMOT_GROUP {
		return e.PubKey, nil
	}
	p := g.cfg.Policy()
	if p.Writes == "open" {
		if len(s.PubKeys) > 0 {
			return s.PubKeys[0], nil
		}
		return e.PubKey, nil
	}
	for _, key := range s.PubKeys {
		a := accessFor(ctx, g.cfg.Community, p, relay.Session{PubKeys: []string{key}})
		if a.Owner || a.Member {
			return key, nil
		}
	}
	return "", fmt.Errorf("auth-required: Marmot group envelopes need an authenticated account allowed to write here")
}

func singletonTag(e event.Event, name string, valid *regexp.Regexp) bool {
	found := 0
	for _, tag := range e.Tags {
		if len(tag) > 0 && tag[0] == name {
			found++
			if len(tag) != 2 || !valid.MatchString(tag[1]) {
				return false
			}
		}
	}
	return found == 1
}
func idList(e event.Event, name string) bool {
	found := 0
	for _, tag := range e.Tags {
		if len(tag) == 0 || tag[0] != name {
			continue
		}
		found++
		if len(tag) < 2 {
			return false
		}
		seen := map[string]bool{}
		for _, id := range tag[1:] {
			if !marmotID.MatchString(id) || seen[id] {
				return false
			}
			seen[id] = true
		}
	}
	return found == 1
}
func countTag(e event.Event, name string) int {
	n := 0
	for _, tag := range e.Tags {
		if tag[0] == name {
			n++
		}
	}
	return n
}
func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

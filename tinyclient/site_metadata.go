package tinyclient

import (
	"math/big"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

const (
	kindSite         = 15128
	kindNamedSite    = 35128
	kindSiteSnapshot = 5128
)

// siteLabel derives the NIP-5A host label for display, without linking the
// storage-backed site server into the frontend. The relay validates manifests.
func siteLabel(e event.Event) string {
	if e.Kind == kindSite {
		if _, err := nostr.PubKeyFromHex(e.PubKey); err != nil {
			return ""
		}
		return identityNpub(e.PubKey)
	}
	key, prefix, suffix := e.PubKey, "", event.Tag(e, "d")
	switch e.Kind {
	case kindNamedSite:
	case kindSiteSnapshot:
		key, prefix, suffix = e.ID, "v", ""
	default:
		return ""
	}
	if !eventIDPattern.MatchString(key) {
		return ""
	}
	n, ok := new(big.Int).SetString(key, 16)
	if !ok {
		return ""
	}
	label := n.Text(36)
	return prefix + strings.Repeat("0", 50-len(label)) + label + suffix
}

func sitePathCount(e event.Event) int {
	count := 0
	for _, tag := range e.Tags {
		if len(tag) > 0 && tag[0] == "path" {
			count++
		}
	}
	return count
}

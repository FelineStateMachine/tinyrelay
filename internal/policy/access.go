package policy

import (
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

type Access struct {
	PubKeys []string
	Role    string
	Member  bool
	Owner   bool
}

func CanRead(p Policy, e event.Event, a Access) bool {
	if containsInt(p.BlockedKinds, e.Kind) {
		return false
	}
	if !readRule(p, a) {
		return false
	}
	if privateKind(e.Kind) {
		return addressed(e, a)
	}
	return true
}

func CanWrite(p Policy, e event.Event, a Access) bool {
	if containsInt(p.BlockedKinds, e.Kind) {
		return false
	}
	if len(p.AllowedKinds) > 0 && !containsInt(p.AllowedKinds, e.Kind) {
		return false
	}
	switch p.Writes {
	case "open":
		return true
	case "owner":
		return a.Owner || (p.Owner != "" && e.PubKey == p.Owner)
	case "allowlist":
		return a.Member || a.Owner
	case "wot":
		return a.Member || a.Owner
	default:
		return false
	}
}

func readRule(p Policy, a Access) bool {
	switch p.Reads {
	case "open":
		return true
	case "auth":
		return len(a.PubKeys) > 0 || a.Owner
	case "members":
		return a.Member || a.Owner
	default:
		return false
	}
}

func privateKind(kind int) bool { return kind == 4 || kind == 1059 }

func addressed(e event.Event, a Access) bool {
	if hasKey(a, e.PubKey) {
		return true
	}
	for _, tag := range e.Tags {
		if len(tag) >= 2 && strings.EqualFold(tag[0], "p") && hasKey(a, tag[1]) {
			return true
		}
	}
	return false
}

func hasKey(a Access, key string) bool {
	for _, candidate := range a.PubKeys {
		if candidate == key {
			return true
		}
	}
	return false
}
func containsInt(values []int, wanted int) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

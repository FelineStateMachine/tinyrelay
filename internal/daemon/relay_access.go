package daemon

import "github.com/FelineStateMachine/tinyrelay/internal/event"

// relayAccessChange identifies tenant membership and moderation events whose
// projections can change the visibility of existing subscriptions.
func relayAccessChange(e event.Event) bool {
	if e.Kind == event.KIND_NIP43_JOIN && event.Tag(e, "claim") == "" {
		return false
	}
	switch e.Kind {
	case event.KIND_REPORT, event.KIND_VANISH, event.KIND_JOIN, event.KIND_LEAVE,
		event.KIND_NIP43_JOIN, event.KIND_NIP43_LEAVE, event.KIND_PUT_USER,
		event.KIND_REMOVE_USER, event.KIND_DELETE_EVENT, event.KIND_DELETE_GROUP:
		return true
	default:
		return false
	}
}

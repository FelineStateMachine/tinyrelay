package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
)

// nip43Invite returns the relay-signed, ephemeral invite response for a
// kind-28935 request. The response is deliberately generated outside storage
// and is only available to an authenticated member allowed by invite policy.
func (t *Tenant) nip43Invite(ctx context.Context, f event.Filter, s relay.Session) (event.Event, bool, error) {
	if !containsKind(f.Kinds, event.KIND_NIP43_INVITE) {
		return event.Event{}, false, nil
	}
	if f.Limit != nil && *f.Limit <= 0 {
		return event.Event{}, false, nil
	}
	now := time.Now().Unix()
	if !nip43FilterCouldMatch(f, t.records.PublicKey(), now) {
		return event.Event{}, false, nil
	}
	if len(s.PubKeys) == 0 {
		return event.Event{}, false, errors.New("auth-required: NIP-43 invite requests require AUTH")
	}
	invite, err := t.community.IssueNIP43Invite(ctx, s.PubKeys[0])
	if err != nil {
		return event.Event{}, false, err
	}
	response, err := t.records.GenerateEphemeral(ctx, event.KIND_NIP43_INVITE, [][]string{{"-"}, {"claim", invite.Code}}, "", now)
	if err != nil {
		return event.Event{}, false, err
	}
	return response, true, nil
}

func nip43FilterCouldMatch(f event.Filter, relayPubKey string, now int64) bool {
	if f.IDs != nil || f.Search != "" {
		return false
	}
	if f.Authors != nil && !membershipContainsString(f.Authors, relayPubKey) {
		return false
	}
	if f.Since != nil && now < *f.Since || f.Until != nil && now > *f.Until {
		return false
	}
	for name, values := range f.Tags {
		if name != "claim" || len(values) == 0 {
			return false
		}
	}
	return true
}

func withoutNIP43Invite(f event.Filter) (event.Filter, bool) {
	if len(f.Kinds) == 0 {
		return f, true
	}
	kinds := make([]int, 0, len(f.Kinds))
	for _, kind := range f.Kinds {
		if kind != event.KIND_NIP43_INVITE {
			kinds = append(kinds, kind)
		}
	}
	if len(kinds) == 0 {
		return event.Filter{}, false
	}
	f.Kinds = kinds
	return f, true
}

func containsKind(kinds []int, wanted int) bool {
	for _, kind := range kinds {
		if kind == wanted {
			return true
		}
	}
	return false
}

func membershipContainsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

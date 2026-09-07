package replication

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type CallbackPolicy struct {
	HostOrigins  []string
	OwnerOrigins []string
}

type PushRegistration struct {
	ID           string
	Owner        string
	Relay        string
	Callback     string
	Filters      []event.Filter
	Ignore       []event.Filter
	IncludeEvent bool
}

func ParsePushRegistration(e event.Event, policy CallbackPolicy, self string) (PushRegistration, error) {
	if e.Kind != event.KIND_PUSH_REGISTRATION {
		return PushRegistration{}, errors.New("replication: wrong push registration kind")
	}
	registration := PushRegistration{ID: e.ID, Owner: e.PubKey}
	registration.Relay = exactTag(e, "relay", self)
	if registration.Relay == "" {
		return PushRegistration{}, errors.New("replication: registration does not name this relay")
	}
	callback := exactOriginTag(e, "callback")
	if callback == "" || !contains(policy.HostOrigins, callback) || !contains(policy.OwnerOrigins, callback) {
		return PushRegistration{}, errors.New("replication: callback origin is not approved")
	}
	registration.Callback = callback
	filters, err := parseFilterTags(e, "filter")
	if err != nil || len(filters) == 0 {
		return PushRegistration{}, errors.New("replication: registration needs a filter")
	}
	registration.Filters = filters
	registration.Ignore, err = parseFilterTags(e, "ignore")
	if err != nil {
		return PushRegistration{}, err
	}
	registration.IncludeEvent = hasTag(e, "include_event")
	return registration, nil
}

func PushMatches(reg PushRegistration, e event.Event) bool {
	if !matchesAny(reg.Filters, e) {
		return false
	}
	return !matchesAny(reg.Ignore, e)
}

func CallbackPayload(reg PushRegistration, e event.Event) ([]byte, error) {
	value := map[string]any{"id": e.ID, "relay": reg.Relay}
	if reg.IncludeEvent {
		value["event"] = e
	}
	return json.Marshal(value)
}

// PrepareCallbackIntents persists references only. Callback I/O is performed
// later by a worker after registration and event visibility are rechecked.
func PrepareCallbackIntents(ctx context.Context, e event.Event, registrations []PushRegistration) []storage.Intent {
	if ctx.Err() != nil || privateKind(e.Kind) || hasProtectedTag(e) {
		return nil
	}
	out := make([]storage.Intent, 0, len(registrations))
	for _, registration := range registrations {
		if !PushMatches(registration, e) {
			continue
		}
		payload, err := json.Marshal(struct {
			Registration PushRegistration `json:"registration"`
		}{registration})
		if err != nil {
			continue
		}
		out = append(out, storage.Intent{Kind: "callback", EventID: e.ID, Target: registration.ID, Payload: string(payload)})
	}
	return out
}

func parseFilterTags(e event.Event, name string) ([]event.Filter, error) {
	values := event.TagValues(e, name)
	filters := make([]event.Filter, 0, len(values))
	for _, value := range values {
		filter, err := event.ParseFilter([]byte(value))
		if err != nil {
			return nil, errors.New("replication: invalid push filter")
		}
		filters = append(filters, filter)
	}
	return filters, nil
}

func exactTag(e event.Event, name, wanted string) string {
	for _, value := range event.TagValues(e, name) {
		if strings.TrimRight(value, "/") == strings.TrimRight(wanted, "/") && safeRelayURL(value) {
			return strings.TrimRight(value, "/")
		}
	}
	return ""
}

func exactOriginTag(e event.Event, name string) string {
	for _, value := range event.TagValues(e, name) {
		parsed, err := url.Parse(value)
		if err == nil && parsed.Scheme == "https" && parsed.User == nil && parsed.Port() == "" && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" {
			return parsed.Scheme + "://" + parsed.Host
		}
	}
	return ""
}

func matchesAny(filters []event.Filter, e event.Event) bool {
	for _, filter := range filters {
		if event.Matches(filter, e) {
			return true
		}
	}
	return false
}

func hasTag(e event.Event, name string) bool {
	for _, tag := range e.Tags {
		if len(tag) > 0 && tag[0] == name {
			return true
		}
	}
	return false
}

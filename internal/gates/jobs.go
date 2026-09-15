package gates

// Long tasks use the six job kinds Buzz reserves. A member or agent
// publishes a request (kind 43001) in a room, naming the keys it asks in p
// tags. An asked key answers with accepted (43002), progress (43003) and
// finally a result (43004) or an error (43006); the requester may cancel
// (43005). The relay keeps every event signed by its author; it checks
// that requests, answers and cancels carry the tags the protocol needs,
// that they come from members or agents, and that every answer or cancel
// names a request the relay holds, from a key the request allows.

import (
	"context"
	"errors"
	"fmt"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// JobShape checks the tags of a job request, answer or cancel event. It
// returns nil for other event kinds.
func JobShape(e event.Event) error {
	switch {
	case event.IsJobRequest(e.Kind):
		return jobRequestShape(e)
	case event.IsJobAnswer(e.Kind):
		return jobAnswerShape(e, JobKindName(e.Kind))
	case event.IsJobCancel(e.Kind):
		return jobCancelShape(e)
	}
	return nil
}

// JobKindName names a job kind the way messages and cards refer to it.
func JobKindName(kind int) string {
	switch kind {
	case event.KIND_JOB_REQUEST:
		return "job request"
	case event.KIND_JOB_ACCEPTED:
		return "job accepted"
	case event.KIND_JOB_PROGRESS:
		return "job progress"
	case event.KIND_JOB_RESULT:
		return "job result"
	case event.KIND_JOB_CANCEL:
		return "job cancel"
	case event.KIND_JOB_ERROR:
		return "job error"
	}
	return "job event"
}

// JobRequestID returns the request an answer or cancel names: its first e
// tag. Later e tags on a result reference artifacts.
func JobRequestID(e event.Event) string {
	return event.Tag(e, "e")
}

func jobRequestShape(e event.Event) error {
	if !community.ValidRoomID(event.Tag(e, "h")) {
		return errors.New("invalid: job request must name its room in an h tag")
	}
	asked := 0
	for _, tag := range e.Tags {
		if len(tag) < 2 {
			continue
		}
		switch tag[0] {
		case "p":
			if !jobID(tag[1]) {
				return errors.New("invalid: job request p tags must be hex public keys")
			}
			asked++
		case "e":
			if !jobID(tag[1]) {
				return errors.New("invalid: job request e tag must name the thread root event id")
			}
		}
	}
	if asked == 0 {
		return errors.New("invalid: job request must name at least one asked key in a p tag")
	}
	return nil
}

func jobAnswerShape(e event.Event, what string) error {
	if !jobID(JobRequestID(e)) {
		return fmt.Errorf("invalid: %s must name the job request in an e tag", what)
	}
	if !jobID(event.Tag(e, "p")) {
		return fmt.Errorf("invalid: %s must name the requester in a p tag", what)
	}
	if !community.ValidRoomID(event.Tag(e, "h")) {
		return fmt.Errorf("invalid: %s must keep the request's room in an h tag", what)
	}
	return nil
}

func jobCancelShape(e event.Event) error {
	if !jobID(JobRequestID(e)) {
		return errors.New("invalid: job cancel must name the job request in an e tag")
	}
	if !community.ValidRoomID(event.Tag(e, "h")) {
		return errors.New("invalid: job cancel must keep the request's room in an h tag")
	}
	if p := event.Tag(e, "p"); p != "" && !jobID(p) {
		return errors.New("invalid: job cancel p tag must be a hex public key")
	}
	return nil
}

// jobFeature refuses job kinds while the owner has long tasks switched off.
func jobFeature(p policy.Policy, e event.Event) error {
	if event.IsJobKind(e.Kind) && !p.Features.Jobs {
		return errors.New("restricted: long tasks are switched off on this relay")
	}
	return nil
}

// jobWriter admits job kinds from members and agents only. An agent is a
// member with the agent role, so one membership check covers both.
func jobWriter(e event.Event, a policy.Access) error {
	if event.IsJobKind(e.Kind) && !a.Member && !a.Owner {
		return errors.New("restricted: long tasks are accepted from members and agents")
	}
	return nil
}

// jobReply checks an answer or cancel against the request it names. The
// relay must hold the request and the publisher must be able to read it.
// An answer must come from a key the request asked, name the requester in
// p and keep the request's room; a cancel must come from the requester.
func (g *Gate) jobReply(ctx context.Context, e event.Event, now int64) error {
	if !event.IsJobAnswer(e.Kind) && !event.IsJobCancel(e.Kind) {
		return nil
	}
	what := JobKindName(e.Kind)
	missing := fmt.Errorf("restricted: %s must name a request this relay holds", what)
	if g.cfg.Store == nil {
		return missing
	}
	result, err := g.cfg.Store.Query(ctx, event.Filter{IDs: []string{JobRequestID(e)}}, storage.QueryOptions{Now: now, Access: storage.Access{All: true}, Limit: 1})
	if err != nil {
		return fmt.Errorf("check job request: %w", err)
	}
	if len(result.Events) == 0 || !event.IsJobRequest(result.Events[0].Kind) {
		return missing
	}
	request := result.Events[0]
	if !g.CanSee(ctx, request, relay.Session{PubKeys: []string{e.PubKey}}, nil) {
		return missing
	}
	if event.Tag(e, "h") != event.Tag(request, "h") {
		return fmt.Errorf("invalid: %s must keep the request's room h tag", what)
	}
	if event.IsJobCancel(e.Kind) {
		if e.PubKey != request.PubKey {
			return errors.New("restricted: only the requester may cancel a long task")
		}
		return nil
	}
	if event.Tag(e, "p") != request.PubKey {
		return fmt.Errorf("invalid: %s p tag must name the requester", what)
	}
	if !containsValue(event.TagValues(request, "p"), e.PubKey) {
		return fmt.Errorf("restricted: %s must come from a key the request asked", what)
	}
	return nil
}

func containsValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func jobID(value string) bool {
	return len(value) == 64 && isLowerHex(value)
}

func isLowerHex(value string) bool {
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

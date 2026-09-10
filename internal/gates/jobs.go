package gates

// Long tasks follow NIP-90: a member or agent publishes a job request
// (kind 5000 to 5127 or 5129 to 5999), and a serving agent or member answers it with
// feedback (kind 7000) and a result (the request kind plus 1000). The relay
// keeps every event signed by its author; it only checks that requests,
// feedback and results carry the tags the protocol needs, that they are
// published by members or agents, and that an agent's answers name a
// request the relay holds.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// JobShape checks the tags of a job request, result or feedback event. It
// returns nil for other event kinds.
func JobShape(e event.Event) error {
	switch {
	case event.IsJobRequest(e.Kind):
		return jobRequestShape(e)
	case event.IsJobResult(e.Kind):
		return jobReplyShape(e, "job result")
	case e.Kind == event.KIND_JOB_FEEDBACK:
		if err := jobReplyShape(e, "job feedback"); err != nil {
			return err
		}
		if !event.IsJobFeedbackStatus(event.Tag(e, "status")) {
			return errors.New("invalid: job feedback status must be payment-required, processing, error, success or partial")
		}
	}
	return nil
}

func jobRequestShape(e event.Event) error {
	for _, tag := range e.Tags {
		if len(tag) == 0 || tag[0] != "i" {
			continue
		}
		if len(tag) < 3 || !event.IsJobInputType(tag[2]) {
			return errors.New("invalid: job input i tag needs data and a type of url, event, job or text")
		}
		if (tag[2] == "event" || tag[2] == "job") && !jobID(tag[1]) {
			return errors.New("invalid: job input of type event or job must name an event id")
		}
	}
	if hasTag(e, "encrypted") && !jobID(event.Tag(e, "p")) {
		return errors.New("invalid: encrypted job request must name the service provider in a p tag")
	}
	if bid := event.Tag(e, "bid"); bid != "" && !jobAmount(bid) {
		return errors.New("invalid: job bid must be an amount in millisats")
	}
	return nil
}

func jobReplyShape(e event.Event, what string) error {
	if !jobID(event.Tag(e, "e")) {
		return fmt.Errorf("invalid: %s must name the job request in an e tag", what)
	}
	if !jobID(event.Tag(e, "p")) {
		return fmt.Errorf("invalid: %s must name the requester in a p tag", what)
	}
	if amount := event.Tag(e, "amount"); amount != "" && !jobAmount(amount) {
		return fmt.Errorf("invalid: %s amount must be in millisats", what)
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

// jobReply checks a result or feedback against the request it names. When
// the relay holds the request, the result kind must match it and the p tag
// must name its author. An agent may only answer a request the relay holds
// and the agent may read; a member may also answer requests made elsewhere.
func (g *Gate) jobReply(ctx context.Context, e event.Event, now int64, agent bool) error {
	if !event.IsJobResult(e.Kind) && e.Kind != event.KIND_JOB_FEEDBACK {
		return nil
	}
	missing := errors.New("restricted: agent grant allows job results and feedback only for requests this relay holds")
	if g.cfg.Store == nil {
		if agent {
			return missing
		}
		return nil
	}
	result, err := g.cfg.Store.Query(ctx, event.Filter{IDs: []string{event.Tag(e, "e")}}, storage.QueryOptions{Now: now, Access: storage.Access{All: true}, Limit: 1})
	if err != nil {
		return fmt.Errorf("check job request: %w", err)
	}
	if len(result.Events) == 0 || !event.IsJobRequest(result.Events[0].Kind) {
		if agent {
			return missing
		}
		return nil
	}
	request := result.Events[0]
	if agent && !g.CanSee(ctx, request, relay.Session{PubKeys: []string{e.PubKey}}, nil) {
		return missing
	}
	if event.IsJobResult(e.Kind) && e.Kind != event.JobResultKind(request.Kind) {
		return fmt.Errorf("invalid: job result kind %d does not answer a kind %d request", e.Kind, request.Kind)
	}
	if event.Tag(e, "p") != request.PubKey {
		return errors.New("invalid: job result or feedback p tag must name the requester")
	}
	return nil
}

func jobID(value string) bool {
	return len(value) == 64 && isLowerHex(value)
}

func jobAmount(value string) bool {
	_, err := strconv.ParseUint(value, 10, 64)
	return err == nil
}

func isLowerHex(value string) bool {
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

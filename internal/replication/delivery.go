package replication

import (
	"context"
	"errors"
	"fmt"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type DeliveryHandler func(context.Context, storage.Intent) error

func NewDeliveryHandler(store *storage.Store, transport DeliveryTransport) DeliveryHandler {
	return func(ctx context.Context, intent storage.Intent) error {
		return deliver(ctx, store, transport, intent)
	}
}

func deliver(ctx context.Context, store *storage.Store, transport DeliveryTransport, intent storage.Intent) error {
	if err := ValidateIntent(intent); err != nil {
		return err
	}
	result, err := store.Query(ctx, event.Filter{IDs: []string{intent.EventID}, Tags: map[string][]string{}}, storage.QueryOptions{Now: 0, Access: storage.Access{All: true}, Limit: 1})
	if err != nil {
		return fmt.Errorf("replication: load delivery event: %w", err)
	}
	if len(result.Events) == 0 {
		return nil
	}
	e := result.Events[0]
	if privateKind(e.Kind) || hasProtectedTag(e) {
		return nil
	}
	if e.Kind != 10002 {
		if relayList, err := authorRelayList(ctx, store, e); err != nil {
			return fmt.Errorf("replication: load author relay list: %w", err)
		} else if relayList != nil {
			if err := sendAccepted(ctx, transport, intent.Target, *relayList); err != nil {
				return err
			}
		}
	}
	return sendAccepted(ctx, transport, intent.Target, e)
}

func authorRelayList(ctx context.Context, store *storage.Store, e event.Event) (*event.Event, error) {
	result, err := store.Query(ctx, event.Filter{Authors: []string{e.PubKey}, Kinds: []int{10002}, Tags: map[string][]string{}}, storage.QueryOptions{Now: 0, Access: storage.Access{All: true}, Limit: 1})
	if err != nil {
		return nil, err
	}
	for _, candidate := range result.Events {
		if candidate.Kind == 10002 && !privateKind(candidate.Kind) && !hasProtectedTag(candidate) {
			return &candidate, nil
		}
	}
	return nil, nil
}

func sendAccepted(ctx context.Context, transport DeliveryTransport, target string, e event.Event) error {
	response, err := transport.Send(ctx, target, e)
	if err != nil {
		return fmt.Errorf("replication: send event: %w", err)
	}
	if response.Accepted || isDuplicate(response.Message) {
		return nil
	}
	message := response.Message
	if message == "" {
		message = "target rejected event"
	}
	return errors.New(message)
}

func hasProtectedTag(e event.Event) bool {
	for _, tag := range e.Tags {
		if len(tag) > 0 && tag[0] == "-" {
			return true
		}
	}
	return false
}

func isDuplicate(message string) bool {
	const prefix = "duplicate:"
	return len(message) >= len(prefix) && message[:len(prefix)] == prefix
}

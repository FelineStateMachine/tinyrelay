package syncprotocol

import (
	"fmt"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip77/negentropy"
	"fiatjaf.com/nostr/nip77/negentropy/storage/vector"
)

type Item struct {
	ID        string
	Timestamp int64
}

type Result struct {
	Response string
	Have     []string
	Need     []string
	Done     bool
}

type Session struct {
	neg       *negentropy.Negentropy
	initiator bool
}

func NewSession(items []Item, initiator bool) (*Session, error) {
	vec := vector.New()
	for _, item := range items {
		id, err := nostr.IDFromHex(item.ID)
		if err != nil {
			return nil, fmt.Errorf("invalid sync item id: %w", err)
		}
		vec.Insert(nostr.Timestamp(item.Timestamp), id)
	}
	vec.Seal()
	return &Session{neg: negentropy.New(vec, 60_000, initiator, initiator), initiator: initiator}, nil
}

func (s *Session) Start() (string, error) {
	if !s.initiator {
		return "", fmt.Errorf("only the initiator can start a negentropy session")
	}
	return s.neg.Start(), nil
}

func (s *Session) Reconcile(message string) (Result, error) {
	if message == "" {
		return Result{}, fmt.Errorf("empty negentropy message")
	}
	response, err := s.neg.Reconcile(message)
	if err != nil {
		return Result{}, fmt.Errorf("reconcile negentropy message: %w", err)
	}
	result := Result{Response: response, Done: s.initiator && response == ""}
	if s.initiator {
		result.Have = drainIDs(s.neg.Haves)
		result.Need = drainIDs(s.neg.HaveNots)
	}
	return result, nil
}

func drainIDs(ids <-chan nostr.ID) []string {
	if ids == nil {
		return nil
	}
	values := make([]string, 0)
	for id := range ids {
		values = append(values, id.Hex())
	}
	return values
}

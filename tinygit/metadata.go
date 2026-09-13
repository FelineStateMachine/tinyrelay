package tinygit

import (
	"errors"
	"fmt"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
)

// Metadata contains the claims parsed from a repository announcement (kind
// 30617) or state (kind 30618). Author is the event's claimed signer, not a
// resolved repository owner. Announcement fields are not inherited by states.
// A Metadata value conveys neither signature validity nor hosting authority.
type Metadata struct {
	Kind        int
	Author      string
	Identifier  string
	EventID     string
	Private     bool
	Clone       []string
	Relays      []string
	Maintainers []string
	Refs        map[string]string
	Head        string
}

// ParseMetadata checks and parses tinygit's supported repository metadata
// shape: identifier, state ref names and nonzero SHA-1 tips, and symbolic HEAD
// syntax. HEAD may name a branch or tag not present in Refs, including an unborn
// branch. Unknown tags and supported extension fields are ignored.
//
// This is a pure shape/semantic check, not full NIP or GRASP compliance. It does
// not validate the event ID or signature, consult announcements or maintainers,
// authorize hosting, check clone or relay endpoints, or inspect Git objects.
// Use nostr.Validate separately before trusting Author. GitRelay.Validate and
// ValidateImported combine signature validation, this parser and host admission;
// missing Git objects remain a separate pending-state concern.
func ParseMetadata(e nostr.Event) (Metadata, error) {
	if e.Kind != 30617 && e.Kind != 30618 {
		return Metadata{}, fmt.Errorf("unsupported: event kind %d is not GRASP repository metadata", e.Kind)
	}
	id := nostr.Tag(e, "d")
	if !validIdentifier(id) {
		return Metadata{}, errors.New("invalid: repository identifier")
	}
	m := Metadata{Kind: e.Kind, Author: e.PubKey, Identifier: id, EventID: e.ID, Refs: map[string]string{}}
	if e.Kind == 30617 {
		for _, tag := range e.Tags {
			if len(tag) < 2 {
				continue
			}
			switch tag[0] {
			case "private":
				m.Private = tag[1] == "true"
			case "clone":
				m.Clone = append(m.Clone, tag[1:]...)
			case "relays":
				m.Relays = append(m.Relays, tag[1:]...)
			case "maintainers":
				m.Maintainers = append(m.Maintainers, tag[1:]...)
			}
		}
		return m, nil
	}
	for _, tag := range e.Tags {
		if len(tag) == 0 {
			continue
		}
		if tag[0] == "HEAD" {
			if len(tag) != 2 || m.Head != "" {
				return Metadata{}, errors.New("invalid: repository state has multiple HEAD tags")
			}
			m.Head = tag[1]
			continue
		}
		if len(tag) < 2 {
			continue
		}
		if strings.HasPrefix(tag[0], "refs/") {
			if _, duplicate := m.Refs[tag[0]]; duplicate || tag[1] == "" || !isObjectID(tag[1]) || strings.Trim(tag[1], "0") == "" {
				return Metadata{}, fmt.Errorf("invalid: ref %s is not a SHA-1", tag[0])
			}
			m.Refs[tag[0]] = tag[1]
		}
	}
	if err := validateRefs(m.Refs); err != nil {
		return Metadata{}, err
	}
	if err := validateHead(m.Head, m.Refs); err != nil {
		return Metadata{}, err
	}
	return m, nil
}

func validateHead(head string, refs map[string]string) error {
	if head == "" {
		return nil
	}
	if !strings.HasPrefix(head, "ref: ") {
		return errors.New("invalid: HEAD must use ref: syntax")
	}
	ref := strings.TrimPrefix(head, "ref: ")
	if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		return errors.New("invalid: HEAD must name a branch or tag")
	}
	if !validRef(ref) {
		return errors.New("invalid: HEAD ref")
	}
	return nil
}

func validateRefs(refs map[string]string) error {
	for ref, oid := range refs {
		if !validRef(ref) || oid != "" && !isObjectID(oid) {
			return fmt.Errorf("invalid: ref %s", ref)
		}
	}
	return nil
}

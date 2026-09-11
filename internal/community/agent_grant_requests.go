package community

// Agent grant requests are deliberately ordinary NIP-22 comments.  The
// request describes a narrow change to the current grant; the operator's
// signed 30392 replacement remains the authority.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

const agentGrantReasonMax = 500

type grantRequestReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func grantRequestOpen(ctx context.Context, db grantRequestReader, request event.Event, now int64) error {
	if expires := event.Expiration(request); expires != 0 && expires <= now {
		return errors.New("conflict: grant request has expired")
	}
	rows, err := db.QueryContext(ctx, `SELECT json_extract(raw,'$.content') FROM events AS reaction
		WHERE kind=7 AND pubkey=? AND (expires=0 OR expires>?) AND EXISTS(
		SELECT 1 FROM tags WHERE event_id=reaction.id AND name='e' AND value=?)`, event.Tag(request, "p"), now, request.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return err
		}
		if strings.TrimSpace(content) == "-" {
			return errors.New("conflict: grant request was denied")
		}
	}
	return rows.Err()
}

type AgentGrantChanges struct {
	Kinds []int       `json:"kinds,omitempty"`
	Rooms []string    `json:"rooms,omitempty"`
	Repos []AgentRepo `json:"repos,omitempty"`
	Sites []AgentSite `json:"sites,omitempty"`
	Wiki  *string     `json:"wiki,omitempty"`
	Jobs  *string     `json:"jobs,omitempty"`
	Rate  *int        `json:"rate,omitempty"`
}

type AgentGrantRequest struct {
	Base    string            `json:"base"`
	Changes AgentGrantChanges `json:"changes"`
}

type AgentGrantReview struct {
	RequestID string      `json:"request_id"`
	Base      string      `json:"base"`
	Agent     string      `json:"agent"`
	Operator  string      `json:"operator"`
	Before    AgentGrant  `json:"before"`
	After     AgentGrant  `json:"after"`
	Unsigned  event.Event `json:"unsigned"`
}

func IsAgentGrantRequest(e event.Event) bool {
	return e.Kind == 1111 && event.Tag(e, "request") == "grant"
}

func decodeGrantRequest(e event.Event) (AgentGrantRequest, error) {
	if !IsAgentGrantRequest(e) {
		return AgentGrantRequest{}, errors.New("invalid: not an agent grant request")
	}
	if strings.TrimSpace(e.Content) == "" || len([]rune(e.Content)) > agentGrantReasonMax {
		return AgentGrantRequest{}, fmt.Errorf("invalid: grant request rationale must be 1 to %d characters", agentGrantReasonMax)
	}
	if !utf8.ValidString(e.Content) {
		return AgentGrantRequest{}, errors.New("invalid: grant request rationale is not valid UTF-8")
	}
	var req AgentGrantRequest
	dec := json.NewDecoder(strings.NewReader(event.Tag(e, "grant")))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Base == "" {
		return AgentGrantRequest{}, errors.New("invalid: grant request needs a base and changes")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return AgentGrantRequest{}, errors.New("invalid: grant request JSON has trailing data")
	}
	allowed := map[string]bool{"request": true, "grant": true, "E": true, "K": true, "P": true, "e": true, "k": true, "p": true, "subject": true, "expiration": true}
	seen := map[string]bool{}
	for _, tag := range e.Tags {
		if len(tag) == 0 || !allowed[tag[0]] {
			return AgentGrantRequest{}, errors.New("invalid: grant request contains an unsupported tag")
		}
		if seen[tag[0]] {
			return AgentGrantRequest{}, errors.New("invalid: grant request contains duplicate tags")
		}
		seen[tag[0]] = true
	}
	if req.Changes.Kinds == nil && req.Changes.Rooms == nil && req.Changes.Repos == nil && req.Changes.Sites == nil && req.Changes.Wiki == nil && req.Changes.Jobs == nil && req.Changes.Rate == nil {
		return AgentGrantRequest{}, errors.New("invalid: grant request has no changes")
	}
	if err := validateGrantChanges(req.Changes); err != nil {
		return AgentGrantRequest{}, err
	}
	return req, nil
}

func validateGrantChanges(changes AgentGrantChanges) error {
	for _, room := range changes.Rooms {
		if !ValidRoomID(room) {
			return errors.New("invalid: grant request room")
		}
	}
	repos, sites := map[string]bool{}, map[string]bool{}
	for _, repo := range changes.Repos {
		key := repo.Owner + ":" + repo.Identifier
		if repos[key] {
			return errors.New("invalid: repeated repository in grant request")
		}
		repos[key] = true
	}
	for _, site := range changes.Sites {
		if sites[site.Label] || site.TTLDays < 0 {
			return errors.New("invalid: duplicate site or negative TTL in grant request")
		}
		sites[site.Label] = true
	}
	return nil
}

func (s *Service) ReviewAgentGrantRequest(ctx context.Context, e event.Event, now int64) (AgentGrantReview, error) {
	req, err := decodeGrantRequest(e)
	if err != nil {
		return AgentGrantReview{}, err
	}
	if err := grantRequestOpen(ctx, s.store.DB(), e, now); err != nil {
		return AgentGrantReview{}, err
	}
	operator := event.Tag(e, "p")
	if req.Base != event.Tag(e, "E") || req.Base != event.Tag(e, "e") {
		return AgentGrantReview{}, errors.New("invalid: grant request base must match its NIP-22 references")
	}
	if len(event.TagValues(e, "p")) != 1 || operator == "" {
		return AgentGrantReview{}, errors.New("invalid: grant request needs exactly one operator")
	}
	if len(event.Tag(e, "E")) != 64 || event.Tag(e, "e") != event.Tag(e, "E") || event.Tag(e, "K") != "30392" || event.Tag(e, "k") != "30392" || event.Tag(e, "P") != operator || event.Tag(e, "p") != operator {
		return AgentGrantReview{}, errors.New("invalid: grant request needs matching NIP-22 grant references")
	}
	var raw string
	var currentID string
	var paused int
	var revokedAt int64
	if err := s.store.DB().QueryRowContext(ctx, `SELECT event_id,paused,revoked_at FROM agent_grants WHERE agent=?`, e.PubKey).Scan(&currentID, &paused, &revokedAt); err != nil || currentID != req.Base {
		return AgentGrantReview{}, errors.New("conflict: grant request is no longer for the current grant")
	}
	if err := s.store.DB().QueryRowContext(ctx, `SELECT raw FROM events WHERE id=? AND kind=?`, req.Base, event.KIND_AGENT_GRANT).Scan(&raw); err != nil {
		if err := s.store.DB().QueryRowContext(ctx, `SELECT raw FROM agent_grant_revisions WHERE event_id=?`, req.Base).Scan(&raw); err != nil {
			return AgentGrantReview{}, errors.New("invalid: current agent grant not found")
		}
	}
	var beforeEvent event.Event
	if beforeEvent, err = event.Parse([]byte(raw)); err != nil {
		return AgentGrantReview{}, err
	}
	review, err := s.reviewAgainstGrant(e, req, beforeEvent, now)
	if err != nil {
		return AgentGrantReview{}, err
	}
	review.Before.Paused = paused != 0
	review.Before.RevokedAt = revokedAt
	if !review.Before.Active(now) {
		return AgentGrantReview{}, errors.New("restricted: grant request is not for the agent's active current grant")
	}
	return review, nil
}

func (s *Service) reviewAgainstGrant(e event.Event, req AgentGrantRequest, beforeEvent event.Event, now int64) (AgentGrantReview, error) {
	operator := event.Tag(e, "p")
	before, err := ParseAgentGrant(beforeEvent, now)
	if err != nil {
		return AgentGrantReview{}, err
	}
	if before.Agent != e.PubKey || before.Owner != operator || !before.Active(now) {
		return AgentGrantReview{}, errors.New("restricted: grant request is not for the agent's active current grant")
	}
	afterEvent := beforeEvent
	afterEvent.Tags = grantRequestTags(beforeEvent.Tags, req.Changes)
	afterEvent.Tags = append(afterEvent.Tags, []string{"grant-request", e.ID}, []string{"grant-base", req.Base}, []string{"e", e.ID, "", "grant-request"})
	afterEvent.ID, afterEvent.Sig, afterEvent.CreatedAt = "", "", now
	if afterEvent.CreatedAt <= beforeEvent.CreatedAt {
		afterEvent.CreatedAt = beforeEvent.CreatedAt + 1
	}
	after, err := ParseAgentGrant(afterEvent, now)
	if err != nil {
		return AgentGrantReview{}, err
	}
	if reflect.DeepEqual(after.Scope, before.Scope) {
		return AgentGrantReview{}, errors.New("invalid: grant request changes nothing")
	}
	return AgentGrantReview{RequestID: e.ID, Base: req.Base, Agent: before.Agent, Operator: operator, Before: before, After: after, Unsigned: afterEvent}, nil
}

func (s *Service) ValidateAgentGrantReplacement(ctx context.Context, e event.Event, now int64) error {
	return s.validateGrantReplacement(ctx, s.store.DB(), e, now)
}

func (s *Service) validateGrantReplacement(ctx context.Context, tx grantRequestReader, e event.Event, now int64) error {
	requestID := event.Tag(e, "grant-request")
	baseID := event.Tag(e, "grant-base")
	if requestID == "" || baseID == "" || !slices.Contains(event.TagValues(e, "e"), requestID) {
		return errors.New("invalid: grant replacement needs grant request provenance")
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT raw FROM events WHERE id=? AND kind=1111`, requestID).Scan(&raw); err != nil {
		return errors.New("invalid: grant request is not stored")
	}
	request, err := event.Parse([]byte(raw))
	if err != nil {
		return err
	}
	if err := grantRequestOpen(ctx, tx, request, now); err != nil {
		return err
	}
	var grantRaw string
	if err := tx.QueryRowContext(ctx, `SELECT raw FROM events WHERE id=? AND kind=?`, baseID, event.KIND_AGENT_GRANT).Scan(&grantRaw); err != nil {
		if err := tx.QueryRowContext(ctx, `SELECT raw FROM agent_grant_revisions WHERE event_id=?`, baseID).Scan(&grantRaw); err != nil {
			return errors.New("invalid: current agent grant not found")
		}
	}
	grantEvent, err := event.Parse([]byte(grantRaw))
	if err != nil {
		return err
	}
	req, err := decodeGrantRequest(request)
	if err != nil {
		return err
	}
	review, err := s.reviewAgainstGrant(request, req, grantEvent, now)
	if err != nil || review.Base != baseID || review.Operator != e.PubKey || review.Agent != event.Tag(e, "d") {
		return errors.New("restricted: grant replacement does not match the reviewed request")
	}
	var paused int
	var revokedAt int64
	if err := tx.QueryRowContext(ctx, `SELECT paused,revoked_at FROM agent_grants WHERE agent=?`, review.Agent).Scan(&paused, &revokedAt); err != nil {
		return errors.New("conflict: agent grant is no longer current")
	}
	if paused != 0 || revokedAt != 0 || review.After.ExpiresAt <= now {
		return errors.New("restricted: grant request is no longer active")
	}
	got, err := ParseAgentGrant(e, now)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(got.Scope, review.After.Scope) || got.ExpiresAt != review.After.ExpiresAt || got.Agent != review.After.Agent || got.Owner != review.After.Owner || got.Name != review.After.Name || e.Content != grantEvent.Content {
		return errors.New("invalid: grant replacement scope differs from the reviewed request")
	}
	if !reflect.DeepEqual(e.Tags, review.Unsigned.Tags) {
		return errors.New("invalid: grant replacement tags differ from the reviewed request")
	}
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT event_id FROM agent_grants WHERE agent=?`, got.Agent).Scan(&current); err != nil || current != baseID {
		return errors.New("conflict: agent grant changed since the request")
	}
	return nil
}

func grantRequestTags(tags [][]string, changes AgentGrantChanges) [][]string {
	replaceRepo := map[string]bool{}
	for _, repo := range changes.Repos {
		replaceRepo[repo.Owner+":"+repo.Identifier] = true
	}
	replaceSite := map[string]bool{}
	for _, site := range changes.Sites {
		replaceSite[site.Label] = true
	}
	copyTags := make([][]string, 0, len(tags)+len(changes.Kinds)+len(changes.Rooms))
	seenKinds := map[string]bool{}
	seenRooms := map[string]bool{}
	remove := map[string]bool{}
	if changes.Wiki != nil {
		remove["wiki"] = true
	}
	if changes.Jobs != nil {
		remove["jobs"] = true
	}
	if changes.Rate != nil {
		remove["rate"] = true
	}
	for _, tag := range tags {
		skip := false
		if len(tag) > 1 && tag[0] == "repo" {
			parts := strings.SplitN(tag[1], ":", 3)
			skip = len(parts) == 3 && replaceRepo[parts[0]+":"+parts[1]]
		}
		if len(tag) > 1 && tag[0] == "sites" {
			skip = replaceSite[tag[1]]
		}
		if len(tag) > 0 && !remove[tag[0]] && !skip && tag[0] != "grant-request" && tag[0] != "grant-base" && !(tag[0] == "e" && len(tag) > 3 && tag[3] == "grant-request") {
			copyTags = append(copyTags, append([]string(nil), tag...))
			if len(tag) > 1 && tag[0] == "k" {
				seenKinds[tag[1]] = true
			}
			if len(tag) > 1 && tag[0] == "room" {
				seenRooms[tag[1]] = true
			}
		}
	}
	// Changes are patches: retain the old entries and add or replace only the
	// requested keys.  Reconstructing these fields also gives deterministic
	// output when a request repeats an existing value.
	for _, k := range changes.Kinds {
		value := fmt.Sprint(k)
		if !seenKinds[value] {
			copyTags = append(copyTags, []string{"k", value})
			seenKinds[value] = true
		}
	}
	for _, room := range changes.Rooms {
		if !seenRooms[room] {
			copyTags = append(copyTags, []string{"room", room})
			seenRooms[room] = true
		}
	}
	for _, repo := range changes.Repos {
		copyTags = append(copyTags, []string{"repo", repo.Owner + ":" + repo.Identifier + ":" + repo.Level})
	}
	for _, site := range changes.Sites {
		tag := []string{"sites", site.Label}
		if site.TTLDays > 0 {
			tag = append(tag, fmt.Sprintf("ttl=%d", site.TTLDays))
		}
		if site.Encrypted {
			tag = append(tag, "encrypted")
		}
		copyTags = append(copyTags, tag)
	}
	if changes.Wiki != nil {
		copyTags = append(copyTags, []string{"wiki", *changes.Wiki})
	}
	if changes.Jobs != nil {
		copyTags = append(copyTags, []string{"jobs", *changes.Jobs})
	}
	if changes.Rate != nil {
		copyTags = append(copyTags, []string{"rate", fmt.Sprint(*changes.Rate)})
	}
	return copyTags
}

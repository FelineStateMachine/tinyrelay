package daemon

// NIP-54 wiki rules that sit on top of stored events: how a merge request is
// read, how its answer is derived from the destination author's reactions,
// and the device notice sent when one arrives. The relay never merges on
// its own; an accepted merge request is applied when the destination author
// publishes a new version of their article.

import (
	"context"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/wiki"
)

const (
	kindWikiArticle  = 30818
	kindWikiMerge    = 818
	kindWikiRedirect = 30819
	kindReaction     = 7
	wikiQueryLimit   = 2000
)

const (
	wikiMergeOpen     = "open"
	wikiMergeAccepted = "accepted"
	wikiMergeRejected = "rejected"
)

type wikiRef struct {
	Coordinate string `json:"a,omitempty"`
	Event      string `json:"e,omitempty"`
}

type wikiVersion struct {
	ID         string   `json:"id"`
	Author     string   `json:"author"`
	CreatedAt  int64    `json:"created_at"`
	D          string   `json:"d"`
	Title      string   `json:"title"`
	Summary    string   `json:"summary,omitempty"`
	Coordinate string   `json:"coordinate"`
	Likes      int      `json:"likes"`
	Fork       *wikiRef `json:"fork,omitempty"`
	Defer      *wikiRef `json:"defer,omitempty"`
	Content    string   `json:"content,omitempty"`
	Links      []string `json:"links,omitempty"`
}

type wikiAnswer struct {
	Status     string `json:"status"`
	AnsweredAt int64  `json:"answered_at,omitempty"`
	Reaction   string `json:"reaction,omitempty"`
}

type wikiMerge struct {
	ID          string `json:"id"`
	Author      string `json:"author"`
	CreatedAt   int64  `json:"created_at"`
	Content     string `json:"content"`
	Target      string `json:"target"`
	TargetD     string `json:"target_d"`
	Destination string `json:"destination"`
	Base        string `json:"base,omitempty"`
	Source      string `json:"source,omitempty"`
	wikiAnswer
}

type wikiRedirect struct {
	ID           string `json:"id"`
	Author       string `json:"author"`
	CreatedAt    int64  `json:"created_at"`
	D            string `json:"d"`
	Target       string `json:"target"`
	TargetD      string `json:"target_d"`
	TargetAuthor string `json:"target_author"`
}

// wikiCoordinate splits a "30818:<pubkey>:<d>" reference.
func wikiCoordinate(value string) (author, d string, ok bool) {
	parts := strings.SplitN(value, ":", 3)
	if len(parts) != 3 || parts[0] != strconv.Itoa(kindWikiArticle) || len(parts[1]) != 64 || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func wikiVersionFrom(e event.Event, full bool) wikiVersion {
	d := event.Tag(e, "d")
	v := wikiVersion{ID: e.ID, Author: e.PubKey, CreatedAt: e.CreatedAt, D: d, Title: event.Tag(e, "title"), Summary: event.Tag(e, "summary"), Coordinate: strconv.Itoa(kindWikiArticle) + ":" + e.PubKey + ":" + d}
	if v.Title == "" {
		v.Title = d
	}
	for _, tag := range e.Tags {
		if len(tag) < 4 || (tag[3] != "fork" && tag[3] != "defer") || (tag[0] != "a" && tag[0] != "e") {
			continue
		}
		ref := &v.Fork
		if tag[3] == "defer" {
			ref = &v.Defer
		}
		if *ref == nil {
			*ref = &wikiRef{}
		}
		if tag[0] == "a" {
			(*ref).Coordinate = tag[1]
		} else {
			(*ref).Event = tag[1]
		}
	}
	if full {
		v.Content = e.Content
		v.Links = wiki.Links(e.Content)
	}
	return v
}

// wikiMergeFrom reads a kind 818 request. The proposed version is the e tag
// with the source marker, or a source tag, or the only plain e tag; when two
// plain e tags are present the first is the base and the last the proposal.
func wikiMergeFrom(e event.Event) wikiMerge {
	m := wikiMerge{ID: e.ID, Author: e.PubKey, CreatedAt: e.CreatedAt, Content: e.Content, Target: event.Tag(e, "a"), wikiAnswer: wikiAnswer{Status: wikiMergeOpen}}
	m.Destination, m.TargetD, _ = wikiCoordinate(m.Target)
	if p := event.Tag(e, "p"); len(p) == 64 {
		m.Destination = p
	}
	var plain []string
	for _, tag := range e.Tags {
		if len(tag) < 2 || tag[0] != "e" || len(tag[1]) != 64 {
			continue
		}
		if len(tag) >= 4 && tag[3] == "source" {
			m.Source = tag[1]
		} else {
			plain = append(plain, tag[1])
		}
	}
	if m.Source == "" {
		if source := event.Tag(e, "source"); len(source) == 64 {
			m.Source = source
		}
	}
	switch {
	case m.Source == "" && len(plain) == 1:
		m.Source = plain[0]
	case m.Source == "" && len(plain) > 1:
		m.Base, m.Source = plain[0], plain[len(plain)-1]
	case len(plain) > 0:
		m.Base = plain[0]
	}
	return m
}

func wikiRedirectFrom(e event.Event) wikiRedirect {
	r := wikiRedirect{ID: e.ID, Author: e.PubKey, CreatedAt: e.CreatedAt, D: event.Tag(e, "d"), Target: event.Tag(e, "a")}
	r.TargetAuthor, r.TargetD, _ = wikiCoordinate(r.Target)
	return r
}

// wikiMergeStatus reports how the destination author answered a merge
// request: a NIP-25 reaction from that pubkey tagging the request with "+"
// accepts it and "-" rejects it. The newest reaction wins.
func (t *Tenant) wikiMergeStatus(ctx context.Context, mergeRequestID, destination string) (wikiAnswer, error) {
	answers, err := t.wikiMergeStatuses(ctx, map[string]string{mergeRequestID: destination})
	if err != nil {
		return wikiAnswer{}, err
	}
	return answers[mergeRequestID], nil
}

// wikiMergeStatuses resolves a set of merge requests, keyed by request id
// with the destination pubkey as value, in one query.
func (t *Tenant) wikiMergeStatuses(ctx context.Context, destinations map[string]string) (map[string]wikiAnswer, error) {
	answers := make(map[string]wikiAnswer, len(destinations))
	ids := make([]string, 0, len(destinations))
	authors := make([]string, 0, len(destinations))
	for id, destination := range destinations {
		answers[id] = wikiAnswer{Status: wikiMergeOpen}
		ids = append(ids, id)
		authors = append(authors, destination)
	}
	if len(ids) == 0 {
		return answers, nil
	}
	rows, err := t.store.Query(ctx, event.Filter{Kinds: []int{kindReaction}, Authors: uniqueStrings(authors), Tags: map[string][]string{"e": ids}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: wikiQueryLimit})
	if err != nil {
		return nil, err
	}
	reactions := rows.Events
	sortEventsNewestFirst(reactions)
	for _, reaction := range reactions {
		status := wikiReactionStatus(reaction.Content)
		if status == "" {
			continue
		}
		for _, id := range event.TagValues(reaction, "e") {
			if destinations[id] != reaction.PubKey || answers[id].Status != wikiMergeOpen {
				continue
			}
			answers[id] = wikiAnswer{Status: status, AnsweredAt: reaction.CreatedAt, Reaction: reaction.ID}
		}
	}
	return answers, nil
}

func wikiReactionStatus(content string) string {
	switch strings.TrimSpace(content) {
	case "+", "":
		return wikiMergeAccepted
	case "-":
		return wikiMergeRejected
	}
	return ""
}

// wikiMemberLikes counts "+" reactions from the owner and members for each
// version, matched by event id or article coordinate.
func (t *Tenant) wikiMemberLikes(ctx context.Context, versions []wikiVersion) (map[string]int, error) {
	likes := make(map[string]int, len(versions))
	if len(versions) == 0 {
		return likes, nil
	}
	ids := make([]string, 0, len(versions))
	coordinates := make([]string, 0, len(versions))
	byCoordinate := make(map[string]string, len(versions))
	for _, v := range versions {
		likes[v.ID] = 0
		ids = append(ids, v.ID)
		coordinates = append(coordinates, v.Coordinate)
		byCoordinate[v.Coordinate] = v.ID
	}
	var reactions []event.Event
	for _, tags := range []map[string][]string{{"e": ids}, {"a": coordinates}} {
		rows, err := t.store.Query(ctx, event.Filter{Kinds: []int{kindReaction}, Tags: tags}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: wikiQueryLimit})
		if err != nil {
			return nil, err
		}
		reactions = append(reactions, rows.Events...)
	}
	members := map[string]bool{t.Policy().Owner: true}
	counted := map[string]bool{}
	for _, reaction := range uniqueEvents(reactions) {
		if wikiReactionStatus(reaction.Content) != wikiMergeAccepted {
			continue
		}
		if _, ok := members[reaction.PubKey]; !ok {
			member, err := t.community.IsMember(ctx, reaction.PubKey)
			if err != nil {
				return nil, err
			}
			members[reaction.PubKey] = member
		}
		if !members[reaction.PubKey] {
			continue
		}
		targets := event.TagValues(reaction, "e")
		for _, coordinate := range event.TagValues(reaction, "a") {
			if id, ok := byCoordinate[coordinate]; ok {
				targets = append(targets, id)
			}
		}
		for _, id := range targets {
			key := reaction.PubKey + "\x00" + id
			if _, known := likes[id]; !known || counted[key] {
				continue
			}
			counted[key] = true
			likes[id]++
		}
	}
	return likes, nil
}

// wikiPreferred picks the version to show: the requested author, else the
// tenant owner, else the most member likes, else the newest. Versions must
// already be sorted newest first.
func wikiPreferred(versions []wikiVersion, author, owner string) (wikiVersion, string) {
	for _, v := range versions {
		if author != "" && v.Author == author {
			return v, "author"
		}
	}
	for _, v := range versions {
		if v.Author == owner {
			return v, "owner"
		}
	}
	best := versions[0]
	for _, v := range versions[1:] {
		if v.Likes > best.Likes {
			best = v
		}
	}
	if best.Likes > 0 {
		return best, "reactions"
	}
	return versions[0], "newest"
}

func sortWikiVersions(versions []wikiVersion) {
	sort.Slice(versions, func(i, j int) bool {
		if versions[i].CreatedAt == versions[j].CreatedAt {
			return versions[i].ID < versions[j].ID
		}
		return versions[i].CreatedAt > versions[j].CreatedAt
	})
}

// notifyWikiMerge wakes the destination author's devices when a merge
// request for one of their articles arrives from someone else.
func (t *Tenant) notifyWikiMerge(ctx context.Context, e event.Event) {
	if e.Kind != kindWikiMerge || !t.pushDevicesExist(ctx) {
		return
	}
	m := wikiMergeFrom(e)
	if m.Destination == "" || m.Destination == e.PubKey || m.TargetD == "" {
		return
	}
	role, err := t.community.Role(ctx, m.Destination)
	if err != nil || role == "" {
		return
	}
	if !t.pushCoalesced(m.Destination, pushReplies) {
		return
	}
	title := m.TargetD
	rows, err := t.store.Query(ctx, event.Filter{Kinds: []int{kindWikiArticle}, Authors: []string{m.Destination}, Tags: map[string][]string{"d": {m.TargetD}}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
	if err == nil && len(rows.Events) == 1 {
		title = wikiVersionFrom(rows.Events[0], false).Title
	}
	notice := pushNotice{recipient: m.Destination, category: pushReplies, body: "Merge request for " + excerpt(title), url: strings.TrimRight(t.publicURL, "/") + "/wiki/" + url.PathEscape(m.TargetD) + "?merge=" + e.ID}
	if err := t.enqueuePushNotice(ctx, notice); err != nil {
		t.app.telemetry.Logger().Debug("wiki merge notification not queued", "error", err)
	}
}

package daemon

// Wiki browsing: the page list, one page with its versions, merge requests
// and redirects, and one merge request. Every read passes the tenant gate
// with the caller's session, so a members-only relay shows nothing to
// guests, and each event is checked with CanSee before it is returned.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/wiki"
)

type wikiBrowseRequest struct {
	D       string `json:"d"`
	Author  string `json:"author"`
	Version string `json:"version"`
	ID      string `json:"id"`
	Event   string `json:"event"`
	Query   string `json:"q"`
	Cursor  string `json:"cursor"`
	Limit   int    `json:"limit"`
}

type wikiPageItem struct {
	D          string      `json:"d"`
	Title      string      `json:"title"`
	Summary    string      `json:"summary,omitempty"`
	Version    wikiVersion `json:"version"`
	Versions   int         `json:"versions"`
	OpenMerges int         `json:"open_merges"`
}

type wikiPage struct {
	D             string         `json:"d"`
	Title         string         `json:"title"`
	Version       *wikiVersion   `json:"version"`
	PreferredBy   string         `json:"preferred_by,omitempty"`
	Versions      []wikiVersion  `json:"versions"`
	Merges        []wikiMerge    `json:"merges"`
	RedirectsTo   []wikiRedirect `json:"redirects_to"`
	RedirectsFrom []wikiRedirect `json:"redirects_from"`
}

type wikiMergeDetail struct {
	Merge    wikiMerge    `json:"merge"`
	Proposed *wikiVersion `json:"proposed"`
	Target   *wikiVersion `json:"target"`
}

func wikiBrowseMethod(method string) bool {
	return method == "browsewiki" || method == "browsewikipage" || method == "browsewikimerge"
}

func (t *Tenant) executeWiki(ctx context.Context, actor, method string, params []json.RawMessage) (any, error) {
	q := wikiBrowseRequest{}
	if len(params) > 0 {
		if err := json.Unmarshal(params[0], &q); err != nil {
			return nil, fmt.Errorf("invalid: wiki parameters: %w", err)
		}
	}
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 50
	}
	if q.Author != "" && len(q.Author) != 64 {
		return nil, errors.New("invalid: wiki author")
	}
	switch method {
	case "browsewiki":
		return t.browseWiki(ctx, actor, q)
	case "browsewikipage":
		return t.browseWikiPage(ctx, actor, q)
	case "browsewikimerge":
		if q.Event == "" {
			q.Event = q.ID
		}
		return t.browseWikiMerge(ctx, actor, q)
	}
	return nil, errors.New("unsupported: browse operation")
}

// wikiQuery reads visible events for the caller's session.
func (t *Tenant) wikiQuery(ctx context.Context, session relay.Session, filters ...event.Filter) ([]event.Event, error) {
	if _, err := t.gate.Read(ctx, filters, session); err != nil {
		return nil, err
	}
	var rows []event.Event
	for _, filter := range filters {
		page, err := t.store.Query(ctx, filter, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{PubKeys: session.PubKeys}, Limit: wikiQueryLimit})
		if err != nil {
			return nil, err
		}
		for _, row := range page.Events {
			if t.gate.CanSee(ctx, row, session, nil) {
				rows = append(rows, row)
			}
		}
	}
	rows = uniqueEvents(rows)
	sortEventsNewestFirst(rows)
	return rows, nil
}

func (t *Tenant) wikiVersions(ctx context.Context, session relay.Session, filter event.Filter) ([]wikiVersion, error) {
	filter.Kinds = []int{kindWikiArticle}
	rows, err := t.wikiQuery(ctx, session, filter)
	if err != nil {
		return nil, err
	}
	versions := make([]wikiVersion, 0, len(rows))
	for _, row := range rows {
		if event.Tag(row, "d") != "" {
			versions = append(versions, wikiVersionFrom(row, false))
		}
	}
	return versions, nil
}

// wikiMerges lists the merge requests aimed at any of the coordinates with
// their answers, newest first.
func (t *Tenant) wikiMerges(ctx context.Context, session relay.Session, coordinates []string) ([]wikiMerge, error) {
	if len(coordinates) == 0 {
		return []wikiMerge{}, nil
	}
	rows, err := t.wikiQuery(ctx, session, event.Filter{Kinds: []int{kindWikiMerge}, Tags: map[string][]string{"a": coordinates}})
	if err != nil {
		return nil, err
	}
	merges := make([]wikiMerge, 0, len(rows))
	destinations := make(map[string]string, len(rows))
	for _, row := range rows {
		m := wikiMergeFrom(row)
		if m.Destination == "" {
			continue
		}
		merges = append(merges, m)
		destinations[m.ID] = m.Destination
	}
	answers, err := t.wikiMergeStatuses(ctx, destinations)
	if err != nil {
		return nil, err
	}
	for i := range merges {
		merges[i].wikiAnswer = answers[merges[i].ID]
	}
	return merges, nil
}

func wikiMatches(v wikiVersion, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	return query == "" || strings.Contains(strings.ToLower(v.Title), query) || strings.Contains(strings.ToLower(v.Summary), query) || strings.Contains(v.D, query)
}

func (t *Tenant) browseWiki(ctx context.Context, actor string, q wikiBrowseRequest) (any, error) {
	session := browseSession(t, actor)
	versions, err := t.wikiVersions(ctx, session, event.Filter{})
	if err != nil {
		return nil, err
	}
	pages := map[string][]wikiVersion{}
	for _, v := range versions {
		if v.D <= q.Cursor || !wikiMatches(v, q.Query) {
			continue
		}
		pages[v.D] = append(pages[v.D], v)
	}
	names := make([]string, 0, len(pages))
	for d := range pages {
		names = append(names, d)
	}
	sort.Strings(names)
	next := ""
	if len(names) > q.Limit {
		names = names[:q.Limit]
		next = names[len(names)-1]
	}
	owner := t.Policy().Owner
	items := make([]wikiPageItem, 0, len(names))
	for _, d := range names {
		page := pages[d]
		if err := t.wikiRankVersions(ctx, page, q.Author, owner); err != nil {
			return nil, err
		}
		preferred, _ := wikiPreferred(page, q.Author, owner)
		merges, err := t.wikiMerges(ctx, session, wikiCoordinates(page))
		if err != nil {
			return nil, err
		}
		open := 0
		for _, m := range merges {
			if m.Status == wikiMergeOpen {
				open++
			}
		}
		items = append(items, wikiPageItem{D: d, Title: preferred.Title, Summary: preferred.Summary, Version: preferred, Versions: len(page), OpenMerges: open})
	}
	return map[string]any{"items": items, "next_cursor": next}, nil
}

// wikiRankVersions fills in member likes when they decide the preferred
// version, which is only when neither an explicit author nor the owner has
// one.
func (t *Tenant) wikiRankVersions(ctx context.Context, versions []wikiVersion, author, owner string) error {
	for _, v := range versions {
		if v.Author == owner || (author != "" && v.Author == author) {
			return nil
		}
	}
	likes, err := t.wikiMemberLikes(ctx, versions)
	if err != nil {
		return err
	}
	for i := range versions {
		versions[i].Likes = likes[versions[i].ID]
	}
	return nil
}

func wikiCoordinates(versions []wikiVersion) []string {
	coordinates := make([]string, 0, len(versions))
	for _, v := range versions {
		coordinates = append(coordinates, v.Coordinate)
	}
	return coordinates
}

func (t *Tenant) browseWikiPage(ctx context.Context, actor string, q wikiBrowseRequest) (any, error) {
	d := wiki.Normalize(q.D)
	if d == "" {
		return nil, errors.New("invalid: wiki page name")
	}
	session := browseSession(t, actor)
	versions, err := t.wikiVersions(ctx, session, event.Filter{Tags: map[string][]string{"d": {d}}})
	if err != nil {
		return nil, err
	}
	redirects, err := t.wikiQuery(ctx, session, event.Filter{Kinds: []int{kindWikiRedirect}, Tags: map[string][]string{"d": {d}}}, event.Filter{Kinds: []int{kindWikiRedirect}, Tags: map[string][]string{"a": wikiCoordinates(versions)}})
	if err != nil {
		return nil, err
	}
	page := wikiPage{D: d, Title: d, Versions: versions, Merges: []wikiMerge{}, RedirectsTo: []wikiRedirect{}, RedirectsFrom: []wikiRedirect{}}
	for _, row := range redirects {
		r := wikiRedirectFrom(row)
		if r.D == d && r.TargetD != "" && r.TargetD != d {
			page.RedirectsFrom = append(page.RedirectsFrom, r)
		} else if r.TargetD == d && r.D != d {
			page.RedirectsTo = append(page.RedirectsTo, r)
		}
	}
	if len(versions) == 0 && len(page.RedirectsFrom) == 0 {
		return nil, errors.New("not found: wiki page")
	}
	if len(versions) == 0 {
		return page, nil
	}
	likes, err := t.wikiMemberLikes(ctx, versions)
	if err != nil {
		return nil, err
	}
	for i := range versions {
		versions[i].Likes = likes[versions[i].ID]
	}
	owner := t.Policy().Owner
	preferred, by := wikiPreferred(versions, q.Author, owner)
	if q.Version != "" {
		by = "version"
		found := false
		for _, v := range versions {
			if v.ID == q.Version {
				preferred, found = v, true
			}
		}
		if !found {
			return nil, errors.New("not found: wiki version")
		}
	}
	rows, err := t.wikiQuery(ctx, session, event.Filter{IDs: []string{preferred.ID}, Limit: intPtr(1)})
	if err != nil {
		return nil, err
	}
	if len(rows) == 1 {
		full := wikiVersionFrom(rows[0], true)
		full.Likes = preferred.Likes
		preferred = full
	}
	page.Title = preferred.Title
	page.Version = &preferred
	page.PreferredBy = by
	page.Merges, err = t.wikiMerges(ctx, session, wikiCoordinates(versions))
	if err != nil {
		return nil, err
	}
	return page, nil
}

func (t *Tenant) browseWikiMerge(ctx context.Context, actor string, q wikiBrowseRequest) (any, error) {
	if len(q.Event) != 64 {
		return nil, errors.New("invalid: wiki merge request")
	}
	session := browseSession(t, actor)
	rows, err := t.wikiQuery(ctx, session, event.Filter{IDs: []string{q.Event}, Kinds: []int{kindWikiMerge}, Limit: intPtr(1)})
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, errors.New("not found: wiki merge request")
	}
	detail := wikiMergeDetail{Merge: wikiMergeFrom(rows[0])}
	if detail.Merge.Destination == "" {
		return nil, errors.New("not found: wiki merge request has no destination")
	}
	detail.Merge.wikiAnswer, err = t.wikiMergeStatus(ctx, detail.Merge.ID, detail.Merge.Destination)
	if err != nil {
		return nil, err
	}
	if detail.Merge.Source != "" {
		proposed, err := t.wikiQuery(ctx, session, event.Filter{IDs: []string{detail.Merge.Source}, Kinds: []int{kindWikiArticle}, Limit: intPtr(1)})
		if err != nil {
			return nil, err
		}
		if len(proposed) == 1 {
			v := wikiVersionFrom(proposed[0], true)
			detail.Proposed = &v
		}
	}
	if detail.Merge.TargetD != "" {
		target, err := t.wikiQuery(ctx, session, event.Filter{Kinds: []int{kindWikiArticle}, Authors: []string{detail.Merge.Destination}, Tags: map[string][]string{"d": {detail.Merge.TargetD}}, Limit: intPtr(1)})
		if err != nil {
			return nil, err
		}
		if len(target) == 1 {
			v := wikiVersionFrom(target[0], true)
			detail.Target = &v
		}
	}
	return detail, nil
}

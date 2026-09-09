package daemon

// Wiki browsing: the page list, one page with its versions, the history
// of its revisions, merge requests and redirects, and one merge request.
// A version's earlier revisions come from the store's wiki_revisions
// table, where a replaced kind 30818 event is kept. Every read passes the tenant gate
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
	// History lists every revision of the page the caller may see across
	// authors, newest first: each author's current version and the
	// archived revisions it replaced.
	History []wikiVersion `json:"history"`
	// CanApprove reports whether the caller may accept or reject proposals.
	CanApprove bool `json:"can_approve"`
}

// wikiSet is what an actor may see of the pages a query matched. Versions
// holds one version per author and page, with a hidden proposal replaced
// by that author's newest approved revision; History holds the visible
// current versions and archived revisions; archived keeps the archived
// events by id for reading their content.
type wikiSet struct {
	Versions []wikiVersion
	History  []wikiVersion
	archived map[string]event.Event
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

// wikiVersions reads the versions and archived revisions the filter
// matches, with each proposal's state filled in and every revision
// numbered, and keeps what the actor may see. A pending or rejected
// proposal stays in place only for the owner, moderators and its author;
// for anyone else the author's newest approved revision stands in for it,
// and the author has no version when there is none.
func (t *Tenant) wikiVersions(ctx context.Context, roles *wikiRoles, actor string, session relay.Session, filter event.Filter) (wikiSet, error) {
	filter.Kinds = []int{kindWikiArticle}
	rows, err := t.wikiQuery(ctx, session, filter)
	if err != nil {
		return wikiSet{}, err
	}
	current := make([]wikiVersion, 0, len(rows))
	ids := make(map[string]bool, len(rows))
	for _, row := range rows {
		if event.Tag(row, "d") != "" {
			current = append(current, wikiVersionFrom(row, false))
			ids[row.ID] = true
		}
	}
	d := ""
	if names := filter.Tags["d"]; len(names) == 1 {
		d = names[0]
	}
	revisions, err := t.store.WikiRevisions(ctx, "", d)
	if err != nil {
		return wikiSet{}, err
	}
	set := wikiSet{Versions: []wikiVersion{}, History: []wikiVersion{}, archived: map[string]event.Event{}}
	archived := make([]wikiVersion, 0, len(revisions))
	for _, r := range revisions {
		if ids[r.ID] || event.Tag(r.Event, "d") == "" || !t.gate.CanSee(ctx, r.Event, session, nil) {
			continue
		}
		v := wikiVersionFrom(r.Event, false)
		v.SupersededBy = r.SupersededBy
		archived = append(archived, v)
		set.archived[v.ID] = r.Event
	}
	if err := t.wikiProposals(ctx, roles, current); err != nil {
		return wikiSet{}, err
	}
	if err := t.wikiProposals(ctx, roles, archived); err != nil {
		return wikiSet{}, err
	}
	wikiNumberRevisions(current, archived)
	decides, err := roles.decides(ctx, actor)
	if err != nil {
		return wikiSet{}, err
	}
	for _, v := range current {
		if wikiSees(decides, actor, v) {
			set.Versions = append(set.Versions, v)
			set.History = append(set.History, v)
		} else if fallback, ok := wikiFallback(v, archived); ok {
			set.Versions = append(set.Versions, fallback)
		}
	}
	for _, v := range archived {
		if wikiSees(decides, actor, v) {
			set.History = append(set.History, v)
		}
	}
	sortWikiVersions(set.Versions)
	sortWikiVersions(set.History)
	return set, nil
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
	roles := t.wikiRoles()
	set, err := t.wikiVersions(ctx, roles, actor, session, event.Filter{})
	if err != nil {
		return nil, err
	}
	decides, err := roles.decides(ctx, actor)
	if err != nil {
		return nil, err
	}
	pages := map[string][]wikiVersion{}
	for _, v := range set.Versions {
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
	return map[string]any{"items": items, "next_cursor": next, "can_approve": decides}, nil
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
	roles := t.wikiRoles()
	set, err := t.wikiVersions(ctx, roles, actor, session, event.Filter{Tags: map[string][]string{"d": {d}}})
	if err != nil {
		return nil, err
	}
	versions := set.Versions
	decides, err := roles.decides(ctx, actor)
	if err != nil {
		return nil, err
	}
	redirects, err := t.wikiQuery(ctx, session, event.Filter{Kinds: []int{kindWikiRedirect}, Tags: map[string][]string{"d": {d}}}, event.Filter{Kinds: []int{kindWikiRedirect}, Tags: map[string][]string{"a": wikiCoordinates(versions)}})
	if err != nil {
		return nil, err
	}
	page := wikiPage{D: d, Title: d, Versions: versions, Merges: []wikiMerge{}, RedirectsTo: []wikiRedirect{}, RedirectsFrom: []wikiRedirect{}, History: set.History, CanApprove: decides}
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
	likes, err := t.wikiMemberLikes(ctx, append(append([]wikiVersion{}, versions...), set.History...))
	if err != nil {
		return nil, err
	}
	for i := range versions {
		versions[i].Likes = likes[versions[i].ID]
	}
	for i := range page.History {
		page.History[i].Likes = likes[page.History[i].ID]
	}
	owner := t.Policy().Owner
	preferred, by := wikiPreferred(versions, q.Author, owner)
	if q.Version != "" {
		// An id names a current version, or an archived revision from the
		// history when it is no longer its author's current version.
		by = "version"
		found := false
		for _, v := range append(append([]wikiVersion{}, versions...), page.History...) {
			if v.ID == q.Version {
				preferred, found = v, true
				break
			}
		}
		if !found {
			return nil, errors.New("not found: wiki version")
		}
	}
	if e, ok := set.archived[preferred.ID]; ok {
		preferred = wikiFull(e, preferred)
	} else {
		rows, err := t.wikiQuery(ctx, session, event.Filter{IDs: []string{preferred.ID}, Limit: intPtr(1)})
		if err != nil {
			return nil, err
		}
		if len(rows) == 1 {
			preferred = wikiFull(rows[0], preferred)
		}
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
		} else if r, ok, err := t.store.WikiRevision(ctx, detail.Merge.Source); err != nil {
			return nil, err
		} else if ok && t.gate.CanSee(ctx, r.Event, session, nil) {
			// The proposed version was replaced since the request was
			// made; the archived revision still shows what was asked.
			v := wikiVersionFrom(r.Event, true)
			v.SupersededBy = r.SupersededBy
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
	// The versions keep their proposal state; a merge request's reader
	// must see the proposed content to answer it, so nothing is hidden.
	roles := t.wikiRoles()
	for _, v := range []*wikiVersion{detail.Proposed, detail.Target} {
		if v == nil {
			continue
		}
		marked := []wikiVersion{*v}
		if err := t.wikiProposals(ctx, roles, marked); err != nil {
			return nil, err
		}
		*v = marked[0]
	}
	return detail, nil
}

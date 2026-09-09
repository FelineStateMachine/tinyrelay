package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type collaborationItem struct {
	ID        string   `json:"id"`
	Kind      int      `json:"kind"`
	Author    string   `json:"author"`
	CreatedAt int64    `json:"created_at"`
	Title     string   `json:"title"`
	Content   string   `json:"content"`
	Labels    []string `json:"labels"`
	Status    string   `json:"status"`
}

type collaborationDetail struct {
	Repository      map[string]any       `json:"repository"`
	Root            event.Event          `json:"root"`
	Item            collaborationItem    `json:"item"`
	Replies         []collaborationReply `json:"replies"`
	Updates         []event.Event        `json:"updates"`
	Statuses        []event.Event        `json:"statuses"`
	Diff            string               `json:"diff,omitempty"`
	CanStatus       bool                 `json:"can_status"`
	DiffUnavailable string               `json:"diff_unavailable,omitempty"`
	DiffTruncated   bool                 `json:"diff_truncated,omitempty"`
	ReplyCursor     string               `json:"reply_cursor,omitempty"`
}

func (t *Tenant) collaborationRepository(ctx context.Context, repo gitrelay.Repository) map[string]any {
	return map[string]any{"owner": repo.Owner, "private": repo.Private, "clone": repo.Clone, "refs": repo.Refs, "head": repo.Head, "maintainers": t.repositoryMaintainers(ctx, repo)}
}

func (t *Tenant) collaborationRepo(ctx context.Context, actor string, q clientBrowseRequest) (gitrelay.Repository, error) {
	if t.git == nil || !t.Policy().Features.Grasp {
		return gitrelay.Repository{}, errors.New("not found: Git hosting is disabled")
	}
	r, err := t.git.BrowseRepository(q.Owner, q.Repo)
	if err != nil {
		return gitrelay.Repository{}, err
	}
	if err := t.browseRepoAccess(ctx, actor, r); err != nil {
		return gitrelay.Repository{}, err
	}
	r.Private = r.Private || t.PrivateServiceEnabled()
	return r, nil
}

func (t *Tenant) browseCollaboration(ctx context.Context, actor string, q clientBrowseRequest, kind int) (any, error) {
	r, err := t.collaborationRepo(ctx, actor, q)
	if err != nil {
		return nil, err
	}
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 50
	}
	cursor, err := parseCollaborationCursor(q.Cursor)
	if err != nil {
		return nil, err
	}
	if q.State != "" && !contains([]string{"open", "resolved", "merged", "closed", "draft"}, q.State) {
		return nil, errors.New("invalid: issue or pull request status")
	}
	coordinate := "30617:" + r.Owner + ":" + r.Identifier
	filters := []event.Filter{{Kinds: []int{kind}, Tags: map[string][]string{"a": {coordinate}}}, {Kinds: []int{kind}, Tags: map[string][]string{"A": {coordinate}}}}
	session := browseSession(t, actor)
	if _, err := t.gate.Read(ctx, filters, session); err != nil {
		return nil, err
	}
	items := make([]collaborationItem, 0, q.Limit)
	next := ""
	for scan := 0; scan < 100; scan++ {
		var rows []event.Event
		more := false
		for _, filter := range filters {
			page, err := t.store.Query(ctx, filter, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{PubKeys: session.PubKeys}, Limit: 100, Before: cursor})
			if err != nil {
				return nil, err
			}
			more = more || page.More
			rows = append(rows, page.Events...)
		}
		rows = uniqueEvents(rows)
		sortEventsNewestFirst(rows)
		if len(rows) > 100 {
			rows = rows[:100]
			more = true
		}
		visible := make([]event.Event, 0, len(rows))
		for _, root := range rows {
			if t.gate.CanSee(ctx, root, session, nil) && collaborationMatches(root, q.Query) && matchesCollaborationLabel(root, q.Label) {
				visible = append(visible, root)
			}
		}
		statuses, err := t.collaborationStatuses(ctx, actor, visible, r)
		if err != nil {
			return nil, err
		}
		for _, root := range rows {
			cursor = &storage.EventCursor{CreatedAt: root.CreatedAt, ID: root.ID}
			status, ok := statuses[root.ID]
			if !ok || (q.State != "" && q.State != status) {
				continue
			}
			if len(items) == q.Limit {
				return map[string]any{"repository": t.collaborationRepository(ctx, r), "items": items, "next_cursor": next}, nil
			}
			item := collaborationItemFrom(root)
			item.Status = status
			items = append(items, item)
			next = collaborationCursor(item)
		}
		if !more {
			return map[string]any{"repository": t.collaborationRepository(ctx, r), "items": items, "next_cursor": ""}, nil
		}
	}
	if cursor != nil {
		next = strconv.FormatInt(cursor.CreatedAt, 10) + ":" + cursor.ID
	}
	return map[string]any{"repository": t.collaborationRepository(ctx, r), "items": items, "next_cursor": next}, nil
}

func parseCollaborationCursor(raw string) (*storage.EventCursor, error) {
	if raw == "" {
		return nil, nil
	}
	stamp, id, ok := strings.Cut(raw, ":")
	ts, err := strconv.ParseInt(stamp, 10, 64)
	if !ok || err != nil || ts < 0 || len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return nil, errors.New("invalid: collaboration cursor")
	}
	return &storage.EventCursor{CreatedAt: ts, ID: id}, nil
}

func matchesCollaborationLabel(root event.Event, label string) bool {
	if label == "" {
		return true
	}
	for _, value := range event.TagValues(root, "t") {
		if strings.EqualFold(value, label) {
			return true
		}
	}
	return false
}

func (t *Tenant) browseCollaborationDetail(ctx context.Context, actor string, q clientBrowseRequest, kind int) (any, error) {
	if len(q.Event) != 64 {
		return nil, errors.New("invalid: collaboration event")
	}
	r, err := t.collaborationRepo(ctx, actor, q)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, []event.Filter{{IDs: []string{q.Event}, Limit: intPtr(1)}}, browseSession(t, actor))
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || rows[0].Kind != kind || !hasCoordinate(rows[0], r) {
		return nil, errors.New("not found: collaboration event")
	}
	root := rows[0]
	detail := collaborationDetail{Repository: t.collaborationRepository(ctx, r), Root: root, Item: collaborationItemFrom(root), CanStatus: actor != "" && (actor == root.PubKey || t.IsMaintainer(ctx, r, actor))}
	replyCursor, err := parseCollaborationCursor(q.Cursor)
	if err != nil {
		return nil, err
	}
	replyLimit := q.Limit
	if replyLimit <= 0 || replyLimit > 200 {
		replyLimit = 200
	}
	authors := append([]string{root.PubKey}, t.maintainerKeys(ctx, r)...)
	statuses, err := t.Query(ctx, []event.Filter{{Authors: authors, Kinds: []int{1630, 1631, 1632, 1633}, Tags: map[string][]string{"e": {root.ID}}, Limit: intPtr(100)}}, browseSession(t, actor))
	if err != nil {
		return nil, err
	}
	detail.Statuses = statuses
	detail.Item.Status = statusFromEvents(root, statuses)
	replyFilters := []event.Filter{
		{Kinds: []int{1111}, Tags: map[string][]string{"E": {root.ID}, "K": {strconv.Itoa(root.Kind)}, "P": {root.PubKey}}},
	}
	replies, err := t.queryCollaborationThread(ctx, actor, replyFilters, replyCursor, replyLimit+1)
	if err != nil {
		return nil, err
	}
	if len(replies) > replyLimit {
		replies = replies[:replyLimit]
		detail.ReplyCursor = collaborationCursor(collaborationItemFrom(replies[replyLimit-1]))
	}
	detail.Replies = collaborationReplies(replies)
	if kind == event.KIND_GIT_PR {
		detail.Updates, err = t.Query(ctx, []event.Filter{{Authors: []string{root.PubKey}, Kinds: []int{1619}, Tags: map[string][]string{"E": {root.ID}, "P": {root.PubKey}}, Limit: intPtr(100)}}, browseSession(t, actor))
		if err != nil {
			return nil, err
		}
		tipEvent := root
		valid := make([]event.Event, 0, len(detail.Updates))
		for _, update := range detail.Updates {
			if event.Tag(update, "P") == root.PubKey && event.Tag(update, "E") == root.ID {
				valid = append(valid, update)
			}
		}
		detail.Updates = valid
		if len(valid) > 0 {
			tipEvent = valid[0]
		}
		diff := t.git.PRDiff(ctx, r, event.Tag(tipEvent, "merge-base"), event.Tag(tipEvent, "c"))
		if diff.Status == "ok" {
			detail.Diff = diff.Diff
			detail.DiffTruncated = diff.Truncated
		} else {
			detail.DiffUnavailable = diff.Reason
		}
	}

	return detail, nil
}

func collaborationItemFrom(e event.Event) collaborationItem {
	return collaborationItem{ID: e.ID, Kind: e.Kind, Author: e.PubKey, CreatedAt: e.CreatedAt, Title: collaborationTitle(e), Content: e.Content, Labels: event.TagValues(e, "t")}
}

func collaborationTitle(e event.Event) string {
	for _, key := range []string{"subject", "title", "name"} {
		if value := event.Tag(e, key); value != "" {
			return value
		}
	}
	content := strings.TrimSpace(e.Content)
	var object struct {
		Title   string `json:"title"`
		Subject string `json:"subject"`
	}
	if json.Unmarshal([]byte(content), &object) == nil {
		if object.Title != "" {
			return object.Title
		}
		if object.Subject != "" {
			return object.Subject
		}
	}
	if line, _, ok := strings.Cut(content, "\n"); ok {
		return line
	}
	return content
}

func collaborationMatches(e event.Event, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	return query == "" || strings.Contains(strings.ToLower(collaborationTitle(e)), query) || strings.Contains(strings.ToLower(e.Content), query) || collaborationLabels(e, query)
}
func collaborationLabels(e event.Event, query string) bool {
	for _, label := range event.TagValues(e, "t") {
		if strings.Contains(strings.ToLower(label), query) {
			return true
		}
	}
	return false
}

// collaborationStatuses resolves the status of every root on a list page with
// one query. Only the root author, the owner and current maintainers can set a
// status, and the newest authorized event wins.
func (t *Tenant) collaborationStatuses(ctx context.Context, actor string, roots []event.Event, r gitrelay.Repository) (map[string]string, error) {
	statuses := make(map[string]string, len(roots))
	if len(roots) == 0 {
		return statuses, nil
	}
	ids := make([]string, 0, len(roots))
	maintainers := t.maintainerKeys(ctx, r)
	authors := append([]string(nil), maintainers...)
	for _, root := range roots {
		ids = append(ids, root.ID)
		authors = append(authors, root.PubKey)
	}
	limit := 1000
	rows, err := t.Query(ctx, []event.Filter{{Authors: uniqueStrings(authors), Kinds: []int{1630, 1631, 1632, 1633}, Tags: map[string][]string{"e": ids}, Limit: &limit}}, browseSession(t, actor))
	if err != nil {
		return nil, err
	}
	byRoot := make(map[string][]event.Event, len(roots))
	for _, row := range rows {
		for _, id := range event.TagValues(row, "e") {
			byRoot[id] = append(byRoot[id], row)
		}
	}
	for _, root := range roots {
		allowed := []event.Event{}
		for _, row := range byRoot[root.ID] {
			if row.PubKey == root.PubKey || contains(maintainers, row.PubKey) {
				allowed = append(allowed, row)
			}
		}
		statuses[root.ID] = statusFromEvents(root, allowed)
	}
	return statuses, nil
}

func (t *Tenant) queryCollaborationThread(ctx context.Context, actor string, filters []event.Filter, cursor *storage.EventCursor, limit int) ([]event.Event, error) {
	session := browseSession(t, actor)
	if _, err := t.gate.Read(ctx, filters, session); err != nil {
		return nil, err
	}
	rows := make([]event.Event, 0)
	for _, filter := range filters {
		page, err := t.store.Query(ctx, filter, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{PubKeys: session.PubKeys}, Limit: limit, Before: cursor})
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
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func statusFromEvents(root event.Event, rows []event.Event) string {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt == rows[j].CreatedAt {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt > rows[j].CreatedAt
	})
	if len(rows) == 0 {
		return "open"
	}
	statuses := map[int]string{1630: "open", 1632: "closed", 1633: "draft"}
	if root.Kind == event.KIND_GIT_ISSUE {
		statuses[1631] = "resolved"
	} else {
		statuses[1631] = "merged"
	}
	return statuses[rows[0].Kind]
}

func collaborationCursor(item collaborationItem) string {
	return strconv.FormatInt(item.CreatedAt, 10) + ":" + item.ID
}

func afterCollaborationCursor(item event.Event, cursor string) bool {
	if cursor == "" {
		return true
	}
	stamp, id, ok := strings.Cut(cursor, ":")
	if !ok {
		return item.ID < cursor
	}
	timestamp, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return false
	}
	return item.CreatedAt < timestamp || (item.CreatedAt == timestamp && item.ID > id)
}

func hasCoordinate(e event.Event, r gitrelay.Repository) bool {
	coordinate := "30617:" + r.Owner + ":" + r.Identifier
	for _, tag := range e.Tags {
		if len(tag) > 1 && (tag[0] == "a" || tag[0] == "A") && tag[1] == coordinate {
			return true
		}
	}
	return false
}
func browseSession(t *Tenant, actor string) relay.Session {
	s := relay.Session{RelayURL: t.RelayURL()}
	if actor != "" {
		s.PubKeys = []string{actor}
	}
	return s
}
func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

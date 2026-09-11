package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type socialBrowseRequest struct {
	Kind    string `json:"kind"`
	Author  string `json:"author"`
	Query   string `json:"q"`
	Cursor  string `json:"cursor"`
	Limit   int    `json:"limit"`
	ID      string `json:"id"`
	Address string `json:"address"`
}

type socialItem struct {
	ID            string           `json:"id"`
	Kind          int              `json:"kind"`
	Author        string           `json:"author"`
	Pubkey        string           `json:"pubkey"`
	Tags          [][]string       `json:"tags,omitempty"`
	CreatedAt     int64            `json:"created_at"`
	Content       string           `json:"content"`
	Title         string           `json:"title,omitempty"`
	Summary       string           `json:"summary,omitempty"`
	Image         string           `json:"image,omitempty"`
	Address       string           `json:"address,omitempty"`
	URL           string           `json:"url,omitempty"`
	ReadPath      string           `json:"read_path"`
	AuthorName    string           `json:"author_name,omitempty"`
	AuthorPicture string           `json:"author_picture,omitempty"`
	ReplyCount    int              `json:"reply_count"`
	Reactions     []socialReaction `json:"reactions,omitempty"`
	ParentID      string           `json:"parent_id,omitempty"`
	ParentAuthor  string           `json:"parent_author,omitempty"`
	ParentKind    int              `json:"parent_kind,omitempty"`
	ParentAddress string           `json:"parent_address,omitempty"`
	RootID        string           `json:"root_id,omitempty"`
	RootAuthor    string           `json:"root_author,omitempty"`
	RootKind      int              `json:"root_kind,omitempty"`
	RootAddress   string           `json:"root_address,omitempty"`
	ReadMinutes   int              `json:"read_minutes,omitempty"`
	Event         event.Event      `json:"event"`
}

type socialReaction struct {
	Content string `json:"content"`
	Count   int    `json:"count"`
	Reacted bool   `json:"reacted,omitempty"`
	Image   string `json:"image,omitempty"`
}

func socialBrowseMethod(method string) bool {
	return method == "browsesocial" || method == "browsesocialthread"
}

func (t *Tenant) executeSocial(ctx context.Context, actor, method string, params []json.RawMessage) (any, error) {
	if !t.Policy().Features.Pages {
		return nil, errors.New("not found: social is disabled")
	}
	q := socialBrowseRequest{}
	if len(params) > 0 {
		if err := json.Unmarshal(params[0], &q); err != nil {
			return nil, fmt.Errorf("invalid: social parameters: %w", err)
		}
	}
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 30
	}
	if q.Author != "" && !socialHex(q.Author) {
		return nil, errors.New("invalid: social author")
	}
	if method == "browsesocialthread" {
		return t.browseSocialThread(ctx, actor, q)
	}
	return t.browseSocial(ctx, actor, q)
}

func socialHex(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}
func socialAddress(row event.Event) string {
	if row.Kind == 30023 {
		return "30023:" + row.PubKey + ":" + event.Tag(row, "d")
	}
	return ""
}
func socialCursor(row event.Event) string {
	return collaborationCursor(collaborationItem{CreatedAt: row.CreatedAt, ID: row.ID})
}
func socialOlder(a, b event.Event) bool {
	if a.CreatedAt == b.CreatedAt {
		return a.ID < b.ID
	}
	return a.CreatedAt > b.CreatedAt
}

func (t *Tenant) browseSocial(ctx context.Context, actor string, q socialBrowseRequest) (any, error) {
	kinds := []int{1, 30023}
	switch q.Kind {
	case "", "all":
	case "notes":
		kinds = []int{1}
	case "articles":
		kinds = []int{30023}
	default:
		return nil, errors.New("invalid: social kind")
	}
	cursor, err := parseCollaborationCursor(q.Cursor)
	if err != nil {
		return nil, err
	}
	filter := event.Filter{Kinds: kinds}
	if q.Author != "" {
		filter.Authors = []string{q.Author}
	}
	rows := []event.Event{}
	next := ""
	session := browseSession(t, actor)
	for scan := 0; scan < 20; scan++ {
		page, err := t.store.Query(ctx, filter, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{PubKeys: session.PubKeys}, Limit: 100, Before: cursor})
		if err != nil {
			return nil, err
		}
		for _, row := range page.Events {
			cursor = &storage.EventCursor{CreatedAt: row.CreatedAt, ID: row.ID}
			if socialFeedRoot(row) && t.gate.CanSee(ctx, row, session, &filter) && socialMatches(row, q.Query) {
				rows = append(rows, row)
			}
			if len(rows) > q.Limit {
				break
			}
		}
		if len(rows) > q.Limit {
			next = socialCursor(rows[q.Limit-1])
			rows = rows[:q.Limit]
			break
		}
		if !page.More || len(page.Events) == 0 {
			break
		}
		if scan == 19 && cursor != nil {
			next = collaborationCursor(collaborationItem{CreatedAt: cursor.CreatedAt, ID: cursor.ID})
		}
	}
	items, err := t.socialItems(ctx, actor, rows)
	if err != nil {
		return nil, err
	}
	stats := map[string]int{"notes": 0, "articles": 0, "comments": 0}
	for _, item := range items {
		if item.Kind == 30023 {
			stats["articles"]++
		} else {
			stats["notes"]++
		}
		stats["comments"] += item.ReplyCount
	}
	return map[string]any{"items": items, "next_cursor": next, "stats": stats}, nil
}

func socialFeedRoot(row event.Event) bool {
	if row.Kind == 30023 {
		return true
	}
	if row.Kind != 1 || event.Tag(row, "h") != "" {
		return false
	}
	ref := socialReference(row)
	return ref.parent == "" && ref.parentAddress == ""
}
func socialMatches(row event.Event, query string) bool {
	needle := strings.ToLower(strings.TrimSpace(query))
	return needle == "" || strings.Contains(strings.ToLower(row.Content+" "+event.Tag(row, "title")+" "+event.Tag(row, "summary")), needle)
}

// socialEvents scans only the requested references and applies the same per-event
// visibility rules as relay subscriptions, including profiles and reactions.
func (t *Tenant) socialEvents(ctx context.Context, actor string, filters []event.Filter) ([]event.Event, error) {
	session := browseSession(t, actor)
	out := []event.Event{}
	seen := map[string]bool{}
	for _, filter := range filters {
		var cursor *storage.EventCursor
		for {
			page, err := t.store.Query(ctx, filter, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{PubKeys: session.PubKeys}, Limit: 200, Before: cursor})
			if err != nil {
				return nil, err
			}
			for _, row := range page.Events {
				if !seen[row.ID] && t.gate.CanSee(ctx, row, session, &filter) {
					seen[row.ID] = true
					out = append(out, row)
				}
			}
			if !page.More || len(page.Events) == 0 {
				break
			}
			last := page.Events[len(page.Events)-1]
			cursor = &storage.EventCursor{CreatedAt: last.CreatedAt, ID: last.ID}
		}
	}
	sort.Slice(out, func(i, j int) bool { return socialOlder(out[i], out[j]) })
	return out, nil
}
func socialActivityFilters(rows []event.Event) []event.Filter {
	ids, addresses := []string{}, []string{}
	for _, row := range rows {
		ids = append(ids, row.ID)
		if address := socialAddress(row); address != "" {
			addresses = append(addresses, address)
		}
	}
	filters := []event.Filter{{Kinds: []int{1, 7, 1111}, Tags: map[string][]string{"e": ids}}, {Kinds: []int{1111}, Tags: map[string][]string{"E": ids}}}
	if len(addresses) > 0 {
		filters = append(filters, event.Filter{Kinds: []int{1, 7, 1111}, Tags: map[string][]string{"a": addresses}}, event.Filter{Kinds: []int{1111}, Tags: map[string][]string{"A": addresses}})
	}
	return filters
}
func (t *Tenant) socialItems(ctx context.Context, actor string, rows []event.Event) ([]socialItem, error) {
	if len(rows) == 0 {
		return []socialItem{}, nil
	}
	extra, err := t.socialEvents(ctx, actor, socialActivityFilters(rows))
	if err != nil {
		return nil, err
	}
	items := socialItemsWithRefs(rows, extra, actor, nil)
	authors := []string{}
	seen := map[string]bool{}
	for _, row := range rows {
		if !seen[row.PubKey] {
			seen[row.PubKey] = true
			authors = append(authors, row.PubKey)
		}
	}
	profiles, err := t.socialEvents(ctx, actor, []event.Filter{{Kinds: []int{0}, Authors: authors}})
	if err != nil {
		return nil, err
	}
	names := map[string][2]string{}
	for _, profile := range profiles {
		var p struct {
			Name    string `json:"name"`
			Display string `json:"display_name"`
			Picture string `json:"picture"`
		}
		if json.Unmarshal([]byte(profile.Content), &p) == nil {
			if p.Display == "" {
				p.Display = p.Name
			}
			names[profile.PubKey] = [2]string{p.Display, p.Picture}
		}
	}
	for i := range items {
		items[i].AuthorName, items[i].AuthorPicture = names[items[i].Pubkey][0], names[items[i].Pubkey][1]
	}
	return items, nil
}

func socialItemsWithRefs(rows, extra []event.Event, actor string, _ map[string]string) []socialItem {
	items := make([]socialItem, len(rows))
	index := map[string]int{}
	for i, row := range rows {
		items[i] = socialItemFrom(row)
		index[row.ID] = i
		if a := socialAddress(row); a != "" {
			index[a] = i
		}
	}
	type reactionKey struct {
		index           int
		author, content string
	}
	seen := map[reactionKey]bool{}
	counts := map[int]map[string]socialReaction{}
	for _, row := range extra {
		if row.Kind == 7 {
			ref := socialReference(row)
			target := ref.parent
			if _, ok := index[ref.parentAddress]; ok {
				target = ref.parentAddress
			}
			i, ok := index[target]
			if !ok {
				continue
			}
			content, image := socialReactionValue(row)
			key := reactionKey{i, row.PubKey, content + "\x00" + image}
			if seen[key] {
				continue
			}
			seen[key] = true
			if counts[i] == nil {
				counts[i] = map[string]socialReaction{}
			}
			variant := content + "\x00" + image
			r := counts[i][variant]
			r.Content = content
			r.Image = image
			r.Count++
			r.Reacted = r.Reacted || row.PubKey == actor
			counts[i][variant] = r
		} else if row.Kind == 1 || row.Kind == 1111 {
			for i, post := range rows {
				if socialCommentFor(row, post) {
					items[i].ReplyCount++
				}
			}
		}
	}
	for i, group := range counts {
		for _, r := range group {
			items[i].Reactions = append(items[i].Reactions, r)
		}
		sort.Slice(items[i].Reactions, func(a, b int) bool {
			if items[i].Reactions[a].Content == items[i].Reactions[b].Content {
				return items[i].Reactions[a].Image < items[i].Reactions[b].Image
			}
			return items[i].Reactions[a].Content < items[i].Reactions[b].Content
		})
	}
	return items
}

// socialReactionValue applies NIP-30 custom emoji metadata only when the
// reaction content and emoji tag form a valid, safe pair. Invalid metadata is
// retained as ordinary reaction text so it cannot turn into an image URL.
func socialReactionValue(row event.Event) (string, string) {
	content := row.Content
	if content == "" {
		return "+", ""
	}
	if len(content) < 3 || content[0] != ':' || content[len(content)-1] != ':' {
		return content, ""
	}
	shortcode := content[1 : len(content)-1]
	if !socialEmojiShortcode(shortcode) {
		return content, ""
	}
	for _, tag := range row.Tags {
		if len(tag) < 3 || tag[0] != "emoji" || tag[1] != shortcode {
			continue
		}
		if socialEmojiURL(tag[2]) {
			return content, tag[2]
		}
	}
	return content, ""
}

func socialEmojiShortcode(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

func socialEmojiURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Hostname() != "" && parsed.User == nil
}

type socialRef struct {
	root, parent, rootAuthor, parentAuthor string
	rootKind, parentKind                   int
	rootAddress, parentAddress             string
}

func tagValue(tag []string, index int) string {
	if len(tag) > index {
		return tag[index]
	}
	return ""
}
func atoiTag(value string) int { n, _ := strconv.Atoi(value); return n }
func addressAuthor(address string) string {
	parts := strings.SplitN(address, ":", 3)
	if len(parts) == 3 {
		return parts[1]
	}
	return ""
}
func socialReference(row event.Event) socialRef {
	ref := socialRef{}
	if row.Kind == 1 {
		var legacy [][]string
		marked := false
		for _, tag := range row.Tags {
			if len(tag) < 2 {
				continue
			}
			if tag[0] == "a" {
				ref.parentAddress = tag[1]
				ref.rootAddress = tag[1]
				continue
			}
			if tag[0] != "e" {
				continue
			}
			switch tagValue(tag, 3) {
			case "root":
				ref.root, ref.rootAuthor = tag[1], tagValue(tag, 4)
				marked = true
			case "reply":
				ref.parent, ref.parentAuthor = tag[1], tagValue(tag, 4)
				marked = true
			case "":
				legacy = append(legacy, tag)
			}
		}
		if !marked && len(legacy) > 0 {
			ref.root = legacy[0][1]
			ref.parent = legacy[len(legacy)-1][1]
		}
		if ref.root == "" {
			ref.root, ref.rootAuthor = ref.parent, ref.parentAuthor
		}
		if ref.parent == "" {
			ref.parent, ref.parentAuthor = ref.root, ref.rootAuthor
		}
		ref.rootKind, ref.parentKind = 1, 1
		return ref
	}
	for _, tag := range row.Tags {
		if len(tag) < 2 {
			continue
		}
		switch tag[0] {
		case "E":
			ref.root, ref.rootAuthor = tag[1], tagValue(tag, 3)
		case "e":
			ref.parent, ref.parentAuthor = tag[1], tagValue(tag, 3)
		case "A":
			ref.rootAddress = tag[1]
		case "a":
			ref.parentAddress = tag[1]
		case "K":
			ref.rootKind = atoiTag(tag[1])
		case "k":
			ref.parentKind = atoiTag(tag[1])
		case "P":
			ref.rootAuthor = tag[1]
		case "p":
			ref.parentAuthor = tag[1]
		}
	}
	if ref.rootAddress != "" && ref.rootAuthor == "" {
		ref.rootAuthor = addressAuthor(ref.rootAddress)
	}
	if ref.parentAddress != "" && ref.parentAuthor == "" {
		ref.parentAuthor = addressAuthor(ref.parentAddress)
	}
	return ref
}

func socialCommentFor(comment, post event.Event) bool {
	ref := socialReference(comment)
	if comment.Kind == 1 && post.Kind == 1 {
		return ref.parent == post.ID || ref.root == post.ID
	}
	if post.Kind == 30023 {
		address := socialAddress(post)
		if comment.Kind == 1111 {
			return ref.rootKind == 30023 && (ref.rootAddress == address || ref.root == post.ID)
		}
		// Older long-form clients used kind 1 with an article a tag. Read these
		// comments for compatibility, but new comments always use NIP-22.
		return comment.Kind == 1 && ref.rootAddress == address
	}
	if post.Kind == 1111 {
		return comment.Kind == 1111 && ref.parent == post.ID && ref.parentKind == 1111
	}
	return false
}
func socialItemFrom(row event.Event) socialItem {
	item := socialItem{ID: row.ID, Kind: row.Kind, Author: row.PubKey, Pubkey: row.PubKey, Tags: row.Tags, CreatedAt: row.CreatedAt, Content: row.Content, Title: event.Tag(row, "title"), Summary: event.Tag(row, "summary"), Image: event.Tag(row, "image"), Event: row, Address: socialAddress(row)}
	item.ReadPath = "/social/" + row.ID
	if item.Address != "" {
		item.ReadPath = "/social?address=" + url.QueryEscape(item.Address)
		item.ReadMinutes = maxInt(1, (len(strings.Fields(row.Content))+219)/220)
	}
	ref := socialReference(row)
	item.ParentID, item.ParentAuthor, item.ParentKind, item.ParentAddress = ref.parent, ref.parentAuthor, ref.parentKind, ref.parentAddress
	if ref.root == "" && ref.rootAddress == "" {
		ref.root, ref.rootAuthor, ref.rootKind, ref.rootAddress = row.ID, row.PubKey, row.Kind, item.Address
	}
	item.RootID, item.RootAuthor, item.RootKind, item.RootAddress = ref.root, ref.rootAuthor, ref.rootKind, ref.rootAddress
	return item
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func socialFilter(identifier string) (event.Filter, error) {
	if socialHex(identifier) {
		return event.Filter{IDs: []string{identifier}}, nil
	}
	parts := strings.SplitN(identifier, ":", 3)
	if len(parts) != 3 || parts[0] != "30023" || !socialHex(parts[1]) {
		return event.Filter{}, errors.New("invalid: social post address")
	}
	return event.Filter{Kinds: []int{30023}, Authors: []string{parts[1]}, Tags: map[string][]string{"d": {parts[2]}}}, nil
}
func (t *Tenant) socialPost(ctx context.Context, actor, identifier string) (event.Event, error) {
	filter, err := socialFilter(identifier)
	if err != nil {
		return event.Event{}, err
	}
	rows, err := t.socialEvents(ctx, actor, []event.Filter{filter})
	if err != nil {
		return event.Event{}, err
	}
	if len(rows) != 1 || (rows[0].Kind != 1 && rows[0].Kind != 30023 && rows[0].Kind != 1111) || event.Tag(rows[0], "h") != "" {
		return event.Event{}, errors.New("not found: social post")
	}
	if rows[0].Kind == 1111 && socialReference(rows[0]).rootKind != 30023 {
		return event.Event{}, errors.New("not found: social comment")
	}
	return rows[0], nil
}
func (t *Tenant) browseSocialThread(ctx context.Context, actor string, q socialBrowseRequest) (any, error) {
	identifier := q.ID
	if identifier == "" {
		identifier = q.Address
	}
	post, err := t.socialPost(ctx, actor, identifier)
	if err != nil {
		return nil, err
	}
	items, err := t.socialItems(ctx, actor, []event.Event{post})
	if err != nil {
		return nil, err
	}
	item := items[0]
	if post.Kind == 1111 || post.Kind == 1 && item.ParentID != "" {
		rootIdentifier := item.RootID
		if item.RootAddress != "" {
			rootIdentifier = item.RootAddress
		}
		root, rootErr := t.socialPost(ctx, actor, rootIdentifier)
		if post.Kind == 1111 && rootErr != nil {
			return nil, errors.New("not found: article for comment")
		}
		if rootErr == nil {
			item.RootID, item.RootAuthor, item.RootKind, item.RootAddress = root.ID, root.PubKey, root.Kind, socialAddress(root)
		}
	}
	cursor, err := parseCollaborationCursor(q.Cursor)
	if err != nil {
		return nil, err
	}
	activity, err := t.socialEvents(ctx, actor, socialActivityFilters([]event.Event{post}))
	if err != nil {
		return nil, err
	}
	replies := []event.Event{}
	for _, row := range activity {
		if !socialCommentFor(row, post) {
			continue
		}
		if cursor != nil && (row.CreatedAt > cursor.CreatedAt || row.CreatedAt == cursor.CreatedAt && row.ID <= cursor.ID) {
			continue
		}
		replies = append(replies, row)
	}
	next := ""
	if len(replies) > q.Limit {
		next = socialCursor(replies[q.Limit-1])
		replies = replies[:q.Limit]
	}
	comments, err := t.socialItems(ctx, actor, replies)
	if err != nil {
		return nil, err
	}
	return map[string]any{"post": item, "comments": comments, "next_cursor": next}, nil
}

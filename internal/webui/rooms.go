package webui

import (
	"context"
	"encoding/json"
	"html/template"
	"regexp"
	"sort"
	"strings"
	"time"
)

// roomPath is one rooms page: the list, one room, or one thread in a room.
type roomPath struct {
	tab   string
	id    string
	event string
}

var (
	roomIDPattern   = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
	eventIDPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	roomLinkPattern = regexp.MustCompile(`https?://[^\s<>"']+|(?:web\+)?nostr:[a-z0-9]+`)
)

// roomRoute maps /rooms, /rooms/<id> and /rooms/<id>/thread/<event> to a
// page. The stream path belongs to the daemon and is not a page.
func roomRoute(path string) roomPath {
	if path == "/rooms" {
		return roomPath{tab: "rooms"}
	}
	rest, ok := strings.CutPrefix(path, "/rooms/")
	if !ok {
		return roomPath{}
	}
	parts := strings.Split(rest, "/")
	if !roomIDPattern.MatchString(parts[0]) {
		return roomPath{}
	}
	switch {
	case len(parts) == 1:
		return roomPath{tab: "room", id: parts[0]}
	case len(parts) == 3 && parts[1] == "thread" && eventIDPattern.MatchString(parts[2]):
		return roomPath{tab: "thread", id: parts[0], event: parts[2]}
	}
	return roomPath{}
}

// roomList reads the rooms the viewer may see for the rail. A failure
// leaves the rail empty rather than failing the page.
func (a *App) roomList(ctx context.Context, actor string) []any {
	if reader := a.roomsReader(); reader != nil {
		list, err := reader.ListRooms(ctx, actor, "", 100)
		if err != nil {
			return nil
		}
		return browseRows(roomListValue(list))
	}
	raw, _ := json.Marshal(map[string]any{"limit": 100})
	rows, err := a.readRows(ctx, "browserooms", []json.RawMessage{raw}, actor)
	if err != nil {
		return nil
	}
	return rows
}

// legacyRoomPage is the compatibility decoder for Backend.Query results.
// Typed production adapters use RoomPage from read_contracts.go instead.
type legacyRoomPage struct {
	Members  []any `json:"members"`
	Messages []any `json:"messages"`
	Replies  []any `json:"replies"`
	Edits    []any `json:"edits"`
}

func legacyRoomPageValue(value any) (legacyRoomPage, bool) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return legacyRoomPage{}, false
	}
	var page legacyRoomPage
	if err := json.Unmarshal(encoded, &page); err != nil {
		return legacyRoomPage{}, false
	}
	return page, true
}

// roomSlice normalizes legacy and JSON-shaped backend results into the room
// page contract. The fallback keeps compatibility with in-process adapters
// that expose a field through their older collaboration helper.
func roomSlice(value any, field string) []any {
	if page, ok := legacyRoomPageValue(value); ok {
		switch field {
		case "members":
			return page.Members
		case "messages":
			return page.Messages
		case "replies":
			return page.Replies
		case "edits":
			return page.Edits
		}
	}
	return collaborationSlice(value, field)
}

func roomMembers(value any) []any { return roomSlice(value, "members") }

// memberIndex maps member pubkeys to their rows so a message can carry the
// author's role and agent marker.
func memberIndex(value any) map[string]map[string]any {
	index := map[string]map[string]any{}
	for _, row := range roomMembers(value) {
		member := valueMap(row)
		if pubkey := plainString(member["pubkey"]); pubkey != "" {
			index[pubkey] = member
		}
	}
	return index
}

// replyCounts counts the thread replies on this page by their root id.
func replyCounts(value any) map[string]int {
	counts := map[string]int{}
	for _, row := range roomSlice(value, "messages") {
		e := valueMap(row)
		if plainString(e["kind"]) != "12" {
			continue
		}
		if roots := tagValues(row, "e"); len(roots) > 0 {
			counts[roots[0]]++
		}
	}
	return counts
}

// reaction is one line of a message's reaction summary.
type reaction struct {
	Content string
	Count   int
}

// reactionSummary groups the page's reactions by the message they answer.
// roomEdit is the newest kind 40003 edit aimed at one message.
type roomEdit struct {
	PubKey, Content string
	CreatedAt       int64
	Tags            [][]string
}

// editSummary maps message ids to the newest edit (kind 40003, Buzz's
// in-place message edit) per editing key found in the page's messages and
// replies, so a stranger's edit never shadows the author's own.
func editSummary(value any) map[string]map[string]roomEdit {
	edits := map[string]map[string]roomEdit{}
	for _, field := range []string{"messages", "replies", "edits"} {
		for _, row := range roomSlice(value, field) {
			e := valueMap(row)
			if plainString(e["kind"]) != "40003" {
				continue
			}
			targets := tagValues(row, "e")
			if len(targets) == 0 {
				continue
			}
			target := targets[len(targets)-1]
			edit := roomEdit{PubKey: plainString(e["pubkey"]), Content: plainString(e["content"]), CreatedAt: unixSeconds(e["created_at"]), Tags: roomTags(row)}
			if edits[target] == nil {
				edits[target] = map[string]roomEdit{}
			}
			if prior, found := edits[target][edit.PubKey]; !found || edit.CreatedAt > prior.CreatedAt {
				edits[target][edit.PubKey] = edit
			}
		}
	}
	return edits
}

func reactionSummary(value any) map[string][]reaction {
	counts := map[string]map[string]int{}
	for _, row := range roomSlice(value, "messages") {
		e := valueMap(row)
		if plainString(e["kind"]) != "7" {
			continue
		}
		targets := tagValues(row, "e")
		if len(targets) == 0 {
			continue
		}
		target := targets[len(targets)-1]
		content := strings.TrimSpace(plainString(e["content"]))
		if content == "" || content == "+" {
			content = "+1"
		}
		if counts[target] == nil {
			counts[target] = map[string]int{}
		}
		counts[target][content]++
	}
	summary := map[string][]reaction{}
	for target, byContent := range counts {
		for content, count := range byContent {
			summary[target] = append(summary[target], reaction{Content: content, Count: count})
		}
		sort.Slice(summary[target], func(i, j int) bool {
			if summary[target][i].Count != summary[target][j].Count {
				return summary[target][i].Count > summary[target][j].Count
			}
			return summary[target][i].Content < summary[target][j].Content
		})
	}
	return summary
}

// tagValues lists the second field of every tag with the given name.
func tagValues(value any, name string) []string {
	var out []string
	var tags [][]string
	if encoded, err := json.Marshal(valueMap(value)["tags"]); err == nil {
		_ = json.Unmarshal(encoded, &tags)
	}
	for _, tag := range tags {
		if len(tag) > 1 && tag[0] == name {
			out = append(out, tag[1])
		}
	}
	return out
}

// roomContent renders message text as escaped HTML with http(s) URLs and
// nostr links turned into anchors. Nostr links open through /open so the
// relay resolves them to a page.
func roomContent(text string) template.HTML {
	var out strings.Builder
	last := 0
	for _, match := range roomLinkPattern.FindAllStringIndex(text, -1) {
		out.WriteString(template.HTMLEscapeString(text[last:match[0]]))
		link := text[match[0]:match[1]]
		trimmed := strings.TrimRight(link, ".,;:!?)")
		rest := link[len(trimmed):]
		href := trimmed
		if !strings.HasPrefix(trimmed, "http") {
			href = "/open?target=" + template.URLQueryEscaper(trimmed)
		}
		out.WriteString(`<a href="` + template.HTMLEscapeString(href) + `"`)
		if strings.HasPrefix(trimmed, "http") {
			out.WriteString(` rel="noopener"`)
		}
		out.WriteString(`>` + template.HTMLEscapeString(trimmed) + `</a>` + template.HTMLEscapeString(rest))
		last = match[1]
	}
	out.WriteString(template.HTMLEscapeString(text[last:]))
	return template.HTML(out.String())
}

// age renders how long ago a Unix timestamp was, compactly, for the rail.
func age(value any) string {
	seconds := unixSeconds(value)
	if seconds <= 0 {
		return ""
	}
	elapsed := time.Since(time.Unix(seconds, 0))
	switch {
	case elapsed < time.Minute:
		return "now"
	case elapsed < time.Hour:
		return plainString(int(elapsed.Minutes())) + "m"
	case elapsed < 24*time.Hour:
		return plainString(int(elapsed.Hours())) + "h"
	default:
		return plainString(int(elapsed.Hours()/24)) + "d"
	}
}

// clock renders a message time: the UTC clock time today, otherwise the day
// and time.
func clock(value any) string {
	seconds := unixSeconds(value)
	if seconds <= 0 {
		return ""
	}
	stamp := time.Unix(seconds, 0).UTC()
	if stamp.Format("2006-01-02") == time.Now().UTC().Format("2006-01-02") {
		return stamp.Format("15:04")
	}
	return stamp.Format("Jan 2 15:04")
}

// roomItem is one message as the page shows it: the event's fields, the
// author's markers, the thread state and the reactions it has received.
type roomItem struct {
	ID, PubKey, Kind, Content, Room, Root, Role, Notice string
	CreatedAt, UpdatedAt                                int64
	Agent, InThread, Edited                             bool
	Mentions                                            []string
	Replies                                             int
	Reactions                                           []reaction
	Attachments                                         []roomAttachment
}

// roomItems turns a browse result's messages or replies into view items,
// oldest first. Reactions and edits become summaries on their targets.
func roomItems(value any, field string) []roomItem {
	data := valueMap(value)
	room := plainString(valueMap(data["room"])["id"])
	members := memberIndex(value)
	replies := replyCounts(value)
	reactions := reactionSummary(value)
	edits := editSummary(value)
	rows := roomSlice(value, field)
	items := make([]roomItem, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		item, ok := newRoomItem(rows[i], room, members)
		if !ok {
			continue
		}
		// Only the author edits their own message, and only forward in time.
		if edit, found := edits[item.ID][item.PubKey]; found && edit.CreatedAt >= item.CreatedAt {
			item.Content, item.Attachments, item.Edited = edit.Content, roomAttachments(map[string]any{"content": edit.Content, "tags": edit.Tags}), true
			item.UpdatedAt = edit.CreatedAt
		}
		item.Replies = replies[item.ID]
		item.Reactions = reactions[item.ID]
		item.InThread = field == "replies"
		items = append(items, item)
	}
	return items
}

// roomRoot is the thread page's root message, with the page's replies counted.
func roomRoot(value any) roomItem {
	data := valueMap(value)
	item, _ := newRoomItem(data["root"], plainString(valueMap(data["room"])["id"]), memberIndex(value))
	if edit, found := editSummary(value)[item.ID][item.PubKey]; found && edit.CreatedAt >= item.CreatedAt {
		item.Content, item.Attachments, item.Edited = edit.Content, roomAttachments(map[string]any{"content": edit.Content, "tags": edit.Tags}), true
		item.UpdatedAt = edit.CreatedAt
	}
	item.Replies = len(roomSlice(value, "replies"))
	item.InThread = true
	return item
}

func newRoomItem(row any, room string, members map[string]map[string]any) (roomItem, bool) {
	e := valueMap(row)
	item := roomItem{ID: plainString(e["id"]), PubKey: plainString(e["pubkey"]), Kind: plainString(e["kind"]), Content: plainString(e["content"]), Room: room, CreatedAt: unixSeconds(e["created_at"])}
	item.UpdatedAt = item.CreatedAt
	item.Attachments = roomAttachments(row)
	switch item.Kind {
	case "7", "40003":
		return roomItem{}, false
	case "44100", "44101":
		who := tagValues(row, "p")
		if len(who) == 0 {
			return roomItem{}, false
		}
		item.PubKey = who[0]
		item.Notice = "joined the room"
		if item.Kind == "44101" {
			item.Notice = "left the room"
		}
	case "12":
		if roots := tagValues(row, "e"); len(roots) > 0 {
			item.Root = roots[0]
		}
	}
	if member := members[item.PubKey]; member != nil {
		item.Role = plainString(member["role"])
		item.Agent, _ = member["agent"].(bool)
	}
	if item.Notice == "" {
		for _, mention := range tagValues(row, "p") {
			if eventIDPattern.MatchString(mention) && mention != item.PubKey {
				item.Mentions = append(item.Mentions, mention)
			}
		}
	}
	return item, item.ID != ""
}

// roomAdmin reports whether a room role may administer the room.
func roomAdmin(role any) bool {
	value := plainString(role)
	return value == "owner" || value == "admin"
}

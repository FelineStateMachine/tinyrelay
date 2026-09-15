package tinyclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type ChatActivityReader interface {
	ReadChatActivity(context.Context, string, string, string) (any, error)
}

type chatActivityItem struct {
	Type, ID, Author, Kind, State, Status, Title, Content, Room, Decision string
	CreatedAt, Expires                                                    int64
	Answer                                                                string
	CanDecide, Question                                                   bool
	Native, Multiple                                                      bool
	Freeform                                                              bool
	Interaction                                                           string
	Options                                                               []chatActivityOption
	Progress                                                              string
	Result, Error                                                         *chatActivityResult
	Assignees                                                             []string
	CanCancel                                                             bool
}
type chatActivityOption struct{ ID, Label string }
type chatActivityResult struct {
	ID, Content string
	Artifacts   []chatActivityLink
}
type chatActivityLink struct{ Href, Label string }
type chatActivityPage struct {
	Items                              []chatActivityItem
	Endpoint, Room, Root, Actor, Error string
	// Ask renders the request form: the backend serves long tasks and the
	// viewer is signed in. Agents lists the room's agent members for it.
	Ask    bool
	Agents []chatActivityAgent
}
type chatActivityAgent struct{ PubKey, Operator string }

func chatActivityView(jobs, approvals any, actor, endpoint, room string) []chatActivityItem {
	items := []chatActivityItem{}
	for _, raw := range collaborationSlice(jobs, "items") {
		item := valueMap(raw)
		id := plainString(item["id"])
		if !eventIDPattern.MatchString(id) {
			continue
		}
		title := strings.TrimSpace(plainString(item["subject"]))
		if title == "" {
			title = strings.TrimSpace(plainString(item["content"]))
		}
		if title == "" {
			title = "Agent task"
		}
		row := chatActivityItem{Type: "job", ID: id, Author: plainString(item["requester"]), Kind: "43001", State: plainString(item["state"]), Status: plainString(item["status"]), Title: truncateActivity(title, 100), Content: plainString(item["content"]), Room: room, CreatedAt: unixSeconds(item["created_at"]), Expires: unixSeconds(item["expires"])}
		for _, assignee := range stringValues(item["assignees"]) {
			if eventIDPattern.MatchString(assignee) {
				row.Assignees = append(row.Assignees, assignee)
			}
		}
		if progress := valueMap(item["progress"]); len(progress) > 0 {
			row.Progress = strings.TrimSpace(plainString(progress["content"]))
		}
		if result := valueMap(item["result"]); eventIDPattern.MatchString(plainString(result["id"])) {
			row.Result = &chatActivityResult{ID: plainString(result["id"]), Content: plainString(result["content"])}
			for _, raw := range collaborationSlice(result, "artifacts") {
				if link, ok := jobArtifactLink(valueMap(raw)); ok {
					row.Result.Artifacts = append(row.Result.Artifacts, link)
				}
			}
		}
		if failure := valueMap(item["error"]); eventIDPattern.MatchString(plainString(failure["id"])) {
			row.Error = &chatActivityResult{ID: plainString(failure["id"]), Content: plainString(failure["content"])}
		}
		if row.State == "" {
			row.State = "open"
		}
		if row.Status == "" {
			row.Status = "queued"
		}
		row.CanCancel = actor != "" && row.Author == actor && row.State == "open"
		items = append(items, row)
	}
	for _, raw := range collaborationSlice(approvals, "items") {
		item := valueMap(raw)
		id := plainString(item["id"])
		if !eventIDPattern.MatchString(id) || actor == "" {
			continue
		}
		asker := plainString(item["asker"])
		can := approvalActorMayDecide(item, actor)
		if !can && asker != actor {
			continue
		}
		title := strings.TrimSpace(plainString(item["subject"]))
		if title == "" {
			title = plainString(item["content"])
		}
		state := plainString(item["state"])
		expires := unixSeconds(item["expires"])
		if state == "open" && expires > 0 && expires <= time.Now().Unix() {
			state = "expired"
		}
		answer := valueMap(item["answer"])
		decision := plainString(answer["decision"])
		row := chatActivityItem{Type: "approval", ID: id, Author: asker, Kind: plainString(item["kind"]), State: state, Title: truncateActivity(title, 150), Content: plainString(item["content"]), Answer: plainString(answer["content"]), Room: room, CreatedAt: unixSeconds(item["created_at"]), Expires: expires, CanDecide: can && state == "open", Question: plainString(item["type"]) == "question", Decision: decision}
		rawOptions := collaborationSlice(item, "options")
		if plainString(item["selection"]) != "" && (len(rawOptions) > 0 || eventHasTag(item, "tinyagent")) {
			row.Native = true
			row.Interaction = plainString(item["interaction"])
			row.Question = row.Interaction == "question"
			row.Multiple = plainString(item["selection"]) == "multiple"
			row.Freeform = plainString(item["freeform"]) == "true" || plainString(item["selection"]) == "text"
			for _, rawOption := range rawOptions {
				option := valueMap(rawOption)
				row.Options = append(row.Options, chatActivityOption{ID: plainString(option["id"]), Label: plainString(option["label"])})
			}
			row.Answer = nativeActivityAnswer(row.Answer, row.Options)
		}
		items = append(items, row)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].CreatedAt == items[j].CreatedAt {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt < items[j].CreatedAt
	})
	return items
}

func nativeActivityAnswer(content string, options []chatActivityOption) string {
	if content == "" || len(options) == 0 {
		return content
	}
	var answer struct {
		Choices []string `json:"choices"`
		Text    string   `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &answer); err != nil || answer.Text == "" {
		answer.Text = ""
		if err := json.Unmarshal([]byte(content), &answer.Choices); err != nil {
			answer.Choices = []string{content}
		}
	}
	labels := make(map[string]string, len(options))
	for _, option := range options {
		labels[option.ID] = option.Label
	}
	selected := make([]string, 0, len(answer.Choices))
	for _, choice := range answer.Choices {
		label, ok := labels[choice]
		if !ok {
			return content
		}
		selected = append(selected, label)
	}
	result := strings.Join(selected, "\n")
	if answer.Text != "" {
		if result != "" {
			result += "\n\n"
		}
		result += answer.Text
	}
	if result == "" {
		return content
	}
	return result
}

// jobArtifactLink turns one artifact reference of a result into a link:
// an event id opens the event, a coordinate opens the addressable event
// and an http or https URL opens as given.
func jobArtifactLink(artifact map[string]any) (chatActivityLink, bool) {
	value := strings.TrimSpace(plainString(artifact["value"]))
	switch plainString(artifact["type"]) {
	case "e":
		if eventIDPattern.MatchString(value) {
			return chatActivityLink{Href: "/e/" + value, Label: "Event " + value[:8]}, true
		}
	case "a":
		parts := strings.SplitN(value, ":", 3)
		if len(parts) == 3 && eventIDPattern.MatchString(parts[1]) && parts[2] != "" {
			return chatActivityLink{Href: "/a/" + parts[0] + ":" + parts[1] + ":" + url.PathEscape(parts[2]), Label: parts[2]}, true
		}
	case "r":
		if strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") {
			return chatActivityLink{Href: value, Label: value}, true
		}
	}
	return chatActivityLink{}, false
}

func eventHasTag(item map[string]any, name string) bool {
	for _, tag := range tagValues(item["event"], name) {
		if tag == "1" {
			return true
		}
	}
	return false
}
func truncateActivity(value string, max int) string {
	runes := []rune(value)
	if len(runes) > max {
		return string(runes[:max]) + "…"
	}
	return value
}
func approvalActorMayDecide(item map[string]any, actor string) bool {
	if actor == "" || plainString(item["asker"]) == actor {
		return false
	}
	for _, key := range stringValues(item["asked"]) {
		if key == actor {
			return true
		}
	}
	return false
}
func stringValues(value any) []string {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var values []string
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	return values
}

// chatActivity renders the room page's panel: the cards plus the request
// form with the room's agent members.
func (a *App) chatActivity(ctx context.Context, actor, room, root string) chatActivityPage {
	page := a.chatActivityCards(ctx, actor, room, root)
	if _, ok := a.backend.(ChatActivityReader); ok && actor != "" && page.Error == "" {
		page.Ask = true
		page.Agents = a.roomAgents(ctx, actor, room)
	}
	return page
}

func (a *App) chatActivityCards(ctx context.Context, actor, room, root string) chatActivityPage {
	endpoint := "/chat/activity?room=" + url.QueryEscape(room)
	if root != "" {
		endpoint += "&root=" + url.QueryEscape(root)
	}
	page := chatActivityPage{Endpoint: endpoint, Room: room, Root: root, Actor: actor}
	reader, ok := a.backend.(ChatActivityReader)
	if !ok {
		return page
	}
	result, err := reader.ReadChatActivity(ctx, actor, room, root)
	if err != nil {
		page.Error = "Agent activity is unavailable."
		return page
	}
	data := valueMap(result)
	page.Items = chatActivityView(map[string]any{"items": data["jobs"]}, map[string]any{"items": data["approvals"]}, actor, endpoint, room)
	return page
}

// roomAgents lists the agent members the viewer may see in a room, from the
// same room read the room page uses, sorted by key so the form is stable.
func (a *App) roomAgents(ctx context.Context, actor, room string) []chatActivityAgent {
	var members []any
	if reader := a.roomsReader(); reader != nil {
		page, err := reader.ReadRoom(ctx, actor, room, "", 1)
		if err != nil {
			return nil
		}
		members = roomMembers(roomPageValueFromContract(page))
	} else {
		raw, _ := json.Marshal(map[string]any{"id": room, "limit": 1})
		result, err := a.backend.Query(ctx, "browseroom", []json.RawMessage{raw}, actor)
		if err != nil {
			return nil
		}
		members = roomMembers(result)
	}
	var agents []chatActivityAgent
	for _, row := range members {
		member := valueMap(row)
		pubkey := plainString(member["pubkey"])
		if isAgent, _ := member["agent"].(bool); !isAgent || !eventIDPattern.MatchString(pubkey) {
			continue
		}
		agents = append(agents, chatActivityAgent{PubKey: pubkey, Operator: plainString(member["operator"])})
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].PubKey < agents[j].PubKey })
	return agents
}
func (a *App) chatActivityHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	actor, err := a.resolveActor(r)
	if err != nil {
		http.Error(w, "Sign in to see chat activity.", http.StatusUnauthorized)
		return
	}
	room, root := r.URL.Query().Get("room"), r.URL.Query().Get("root")
	if !roomIDPattern.MatchString(room) || root != "" && !eventIDPattern.MatchString(root) {
		http.Error(w, "Invalid room or thread.", http.StatusBadRequest)
		return
	}
	page := a.chatActivityCards(r.Context(), actor, room, root)
	if page.Error != "" {
		http.Error(w, page.Error, http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var body bytes.Buffer
	if err := a.tmpl.ExecuteTemplate(&body, "chatActivityResponse", page); err != nil {
		http.Error(w, "Activity rendering failed: "+err.Error(), 500)
		return
	}
	_, _ = w.Write([]byte(injectBase(body.String(), requestPrefix(r))))
}

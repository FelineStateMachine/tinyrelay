package webui

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
	CanDecide, Question, Encrypted                                        bool
	Result                                                                *chatActivityResult
}
type chatActivityResult struct{ ID, Content string }
type chatActivityPage struct {
	Items                        []chatActivityItem
	Endpoint, Room, Actor, Error string
}

func chatActivityView(jobs, approvals any, actor, endpoint, room string) []chatActivityItem {
	items := []chatActivityItem{}
	for _, raw := range collaborationSlice(jobs, "items") {
		item := valueMap(raw)
		id := plainString(item["id"])
		if !eventIDPattern.MatchString(id) {
			continue
		}
		ev := valueMap(item["event"])
		title := firstString(tagValues(ev, "subject"))
		if title == "" {
			title = strings.TrimSpace(plainString(item["content"]))
		}
		if title == "" {
			for _, raw := range collaborationSlice(item, "inputs") {
				input := valueMap(raw)
				if plainString(input["type"]) == "text" {
					title = plainString(input["data"])
					break
				}
			}
		}
		if title == "" {
			title = "Agent task"
		}
		title = truncateActivity(title, 100)
		row := chatActivityItem{Type: "job", ID: id, Author: plainString(item["requester"]), Kind: plainString(item["kind"]), State: plainString(item["state"]), Status: plainString(item["status"]), Title: title, Content: plainString(item["content"]), Room: room, CreatedAt: unixSeconds(item["created_at"])}
		row.Encrypted, _ = item["encrypted"].(bool)
		if row.Encrypted {
			row.Title = "Encrypted task"
			row.Content = "Open the task in its originating client."
		}
		if feedback := valueMap(item["feedback"]); len(feedback) > 0 && !row.Encrypted {
			row.Author = plainString(feedback["provider"])
			row.Content = strings.TrimSpace(plainString(feedback["info"]) + "\n" + plainString(feedback["content"]))
		}
		if result := valueMap(item["result"]); eventIDPattern.MatchString(plainString(result["id"])) {
			row.Result = &chatActivityResult{ID: plainString(result["id"]), Content: plainString(result["content"])}
			row.Status = "success"
			if row.Encrypted {
				row.Result.Content = ""
			}
		}
		if row.Status == "" {
			row.Status = "queued"
		}
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
		items = append(items, chatActivityItem{Type: "approval", ID: id, Author: asker, Kind: plainString(item["kind"]), State: state, Title: truncateActivity(title, 150), Content: plainString(item["content"]), Answer: plainString(answer["content"]), Room: room, CreatedAt: unixSeconds(item["created_at"]), Expires: expires, CanDecide: can && state == "open", Question: plainString(item["type"]) == "question", Decision: decision})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].CreatedAt == items[j].CreatedAt {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt < items[j].CreatedAt
	})
	return items
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

func (a *App) chatActivity(ctx context.Context, actor, room, root string) chatActivityPage {
	endpoint := "/chat/activity?room=" + url.QueryEscape(room)
	if root != "" {
		endpoint += "&root=" + url.QueryEscape(root)
	}
	page := chatActivityPage{Endpoint: endpoint, Room: room, Actor: actor}
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
	page := a.chatActivity(r.Context(), actor, room, root)
	if page.Error != "" {
		http.Error(w, page.Error, http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var body bytes.Buffer
	if err := a.tmpl.ExecuteTemplate(&body, "chatActivityResponse", page); err != nil {
		http.Error(w, "Activity rendering failed.", 500)
		return
	}
	_, _ = w.Write([]byte(injectBase(body.String(), requestPrefix(r))))
}

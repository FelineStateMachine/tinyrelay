package webui

import (
	"context"
	"encoding/json"
	"strings"
)

// approvalView is one request as the Approvals page renders it. The backend
// answers browseapprovals with typed items; this flattens what the template
// needs so the markup stays plain.
type approvalView struct {
	ID, Kind, Type, State, Asker string
	CreatedAt, Expires           int64
	// Where the request was made: the room, the repository or the relay.
	Where string
	// Title is the subject, or the content when there is no subject; Body is
	// the content when a subject is present.
	Title, Body string
	// About summarizes the a or e reference in one mono line, and Open is
	// the relay path that shows it.
	About, Open, Coordinate string
	Decision                string
	AnsweredAt              int64
	AnswerContent           string
}

// approvalViews returns the items in the given states, in the order the
// backend listed them.
func approvalViews(value any, base string, states ...string) []approvalView {
	var views []approvalView
	for _, raw := range collaborationSlice(value, "items") {
		item := valueMap(raw)
		state := plainString(item["state"])
		wanted := len(states) == 0
		for _, s := range states {
			wanted = wanted || s == state
		}
		if !wanted {
			continue
		}
		views = append(views, approvalViewFrom(item, base))
	}
	return views
}

func approvalViewFrom(item map[string]any, base string) approvalView {
	view := approvalView{ID: plainString(item["id"]), Kind: plainString(item["kind"]), Type: plainString(item["type"]), State: plainString(item["state"]), Asker: plainString(item["asker"]), CreatedAt: unixSeconds(item["created_at"]), Expires: unixSeconds(item["expires"])}
	subject, content := strings.TrimSpace(plainString(item["subject"])), strings.TrimSpace(plainString(item["content"]))
	view.Title, view.Body = subject, content
	if subject == "" {
		view.Title, view.Body = content, ""
	}
	view.Where = "the relay"
	view.Open = "/e/" + view.ID
	if about := valueMap(item["about"]); len(about) > 0 {
		coordinate, referenced, kind := plainString(about["coordinate"]), plainString(about["event"]), plainString(about["kind"])
		var parts []string
		if coordinate != "" {
			parts = append(parts, "about "+coordinate)
			if fields := strings.SplitN(coordinate, ":", 3); len(fields) == 3 && fields[0] == "30617" {
				view.Coordinate = coordinate
				view.Where = "repository " + fields[2]
			}
		}
		if referenced != "" {
			label := "event " + shortID(referenced)
			if kind != "" {
				label = "kind " + kind + " " + shortID(referenced)
			}
			parts = append(parts, label)
		}
		view.About = strings.Join(parts, " | ")
		if url := plainString(about["url"]); url != "" {
			view.Open = relayPath(url, base)
		}
	}
	if room := plainString(item["room"]); room != "" {
		view.Where = "#" + room
	}
	if answer := valueMap(item["answer"]); len(answer) > 0 {
		view.Decision, view.AnsweredAt, view.AnswerContent = plainString(answer["decision"]), unixSeconds(answer["created_at"]), strings.TrimSpace(plainString(answer["content"]))
	}
	return view
}

// includeApproval makes sure the request a notification opened is on the
// page: when the listing does not carry it, the single request is read and
// placed first. A request that cannot be read leaves the listing as it is.
func (a *App) includeApproval(ctx context.Context, actor string, result any, id string) any {
	page := valueMap(result)
	items := collaborationSlice(result, "items")
	for _, item := range items {
		if plainString(valueMap(item)["id"]) == id {
			return result
		}
	}
	raw, _ := json.Marshal(map[string]any{"id": id})
	detail, err := a.backend.Query(ctx, "browseapproval", []json.RawMessage{raw}, actor)
	if err != nil {
		return result
	}
	item := valueMap(detail)["item"]
	if item == nil {
		return result
	}
	page["items"] = append([]any{item}, items...)
	return page
}

// relayPath turns an absolute relay URL into the path the page links to, so
// tenant prefixes are applied once when the page is written.
func relayPath(url, base string) string {
	base = strings.TrimRight(base, "/")
	if base != "" && strings.HasPrefix(url, base+"/") {
		return strings.TrimPrefix(url, base)
	}
	if i := strings.Index(url, "://"); i >= 0 {
		if slash := strings.Index(url[i+3:], "/"); slash >= 0 {
			return url[i+3+slash:]
		}
		return "/"
	}
	return url
}

// approvalCounts reads the counts object the backend returns beside the
// items, defaulting every state to zero.
func approvalCounts(value any) map[string]int {
	counts := map[string]int{"open": 0, "answered": 0, "expired": 0}
	encoded, err := json.Marshal(valueMap(value)["counts"])
	if err == nil {
		_ = json.Unmarshal(encoded, &counts)
	}
	return counts
}

// approvalDevices lists the caller's devices as "categories" strings.
func approvalDevices(value any) []string {
	var devices []string
	for _, raw := range collaborationSlice(value, "devices") {
		var categories []string
		encoded, err := json.Marshal(valueMap(raw)["categories"])
		if err == nil {
			_ = json.Unmarshal(encoded, &categories)
		}
		if len(categories) == 0 {
			devices = append(devices, "no categories")
			continue
		}
		devices = append(devices, strings.Join(categories, ", "))
	}
	return devices
}

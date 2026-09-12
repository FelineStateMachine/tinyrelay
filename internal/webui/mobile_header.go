package webui

import (
	"net/url"
	"strings"
)

// mobileHeaderData is the small, route-aware context used by the mobile shell.
// It contains only data that has already passed page access checks.
type mobileHeaderData struct {
	Title     string
	Subtitle  string
	BackURL   string
	BackLabel string
	PubKey    string
	RoomID    string
	Picture   string
}

// mobileHeader derives a compact header from rendered page data. Private and
// failed room pages deliberately use generic values so hidden room metadata
// cannot appear in the shell.
func mobileHeader(data PageData) mobileHeaderData {
	if data.Private {
		return mobileHeaderData{Title: "Sign in", Subtitle: data.Slug}
	}
	if data.Tab == "signin" {
		title := "Sign in"
		if data.Actor != "" {
			title = "Account"
			if data.Query.Get("connect") != "" {
				title = "Connect signer"
			}
		}
		result := mobileHeaderData{Title: title, Subtitle: data.Slug, BackLabel: "Back"}
		if next, ok := mobileReturnPath(data.Query.Get("next"), data.Base); ok {
			result.BackURL = next
			result.BackLabel = "Back"
		}
		return result
	}
	if data.Error != "" && roomRoute(data.Path).tab != "" {
		return mobileHeaderData{Title: "Chat", BackURL: "/chat", BackLabel: "Back to chats"}
	}
	route := roomRoute(data.Path)
	switch route.tab {
	case "room":
		return roomHeader(data, route, false)
	case "thread":
		return roomHeader(data, route, true)
	}
	if data.Tab == "direct" || strings.HasPrefix(data.Path, "/chat/dm/") {
		result := mobileHeaderData{Title: "Direct chat", Subtitle: "Private conversation", BackURL: "/chat", BackLabel: "Back to chats"}
		if eventIDPattern.MatchString(data.View) {
			result.PubKey = data.View
		}
		return result
	}
	return generalMobileHeader(data)
}

func roomHeader(data PageData, route roomPath, thread bool) mobileHeaderData {
	result := mobileHeaderData{Title: "Group chat", Subtitle: "Chat", RoomID: route.id}
	room := valueMap(valueMap(data.Event)["room"])
	if name := plainString(room["name"]); name != "" {
		result.Title = name
	}
	result.Picture = socialMediaURL(plainString(room["picture"]))
	if thread {
		result.Title = "Thread"
		result.Subtitle = plainString(room["name"])
		if result.Subtitle == "" {
			result.Subtitle = "Group chat"
		}
		root := plainString(valueMap(valueMap(data.Event)["root"])["id"])
		if !eventIDPattern.MatchString(root) {
			root = route.event
		}
		if eventIDPattern.MatchString(root) {
			result.BackURL = "/rooms/" + route.id + "#msg-" + root
		}
		result.BackLabel = "Back to room"
		return result
	}
	result.BackURL, result.BackLabel = "/chat", "Back to chats"
	return result
}

func generalMobileHeader(data PageData) mobileHeaderData {
	path := data.Path
	result := mobileHeaderData{Title: mobileTitle(path, data), Subtitle: data.Slug, BackLabel: "Back"}
	if data.Tab == "social-thread" {
		result.Title = eventShortTitle(data.Event, "Social")
		result.Subtitle = "Social"
	}
	if data.Tab == "file" {
		result.Title = eventShortTitle(data.Event, "File")
		result.Subtitle = "Files"
	}
	if data.Tab == "files" && data.Query.Get("view") == "sites" {
		result.Title = "Sites"
		result.Subtitle = "Files"
	}
	if data.Tab == "repo" {
		if repo := data.Query.Get("repo"); repo != "" {
			result.Title = repo
		}
		result.Subtitle = repoView(data.Query)
	}
	if data.Tab == "wikipage" {
		result.Title = eventShortTitle(data.Event, data.Query.Get("d"))
		result.Subtitle = "Wiki"
	}
	switch {
	case path == "/" || path == "":
	case path == "/chat" || path == "/social" || path == "/files" || path == "/wiki" || path == "/repos":
		result.BackURL = "/"
	case path == "/manage" || path == "/manage/people":
		result.BackURL = "/"
		result.BackLabel = "Back to home"
	case strings.HasPrefix(path, "/manage/"):
		result.BackURL = "/manage/people"
		result.BackLabel = "Back to manage"
	case path == "/sites":
		result.BackURL = "/"
	case path == "/file":
		result.BackURL = "/files"
		result.BackLabel = "Back to files"
	case path == "/repo":
		result.BackURL = "/repos"
		result.BackLabel = "Back to repositories"
	case path == "/profile":
		result.BackURL = "/account"
		result.BackLabel = "Back to account"
	case strings.HasPrefix(path, "/social/"):
		result.BackURL = "/social"
		result.BackLabel = "Back to social"
	case strings.HasPrefix(path, "/files/"):
		result.BackURL = "/files"
		result.BackLabel = "Back to files"
	case strings.HasPrefix(path, "/wiki/"):
		result.BackURL = "/wiki"
		result.BackLabel = "Back to wiki"
	case strings.HasPrefix(path, "/repos/"):
		result.BackURL = "/repos"
		result.BackLabel = "Back to repositories"
	case path != "/social" && path != "/files" && path != "/wiki" && path != "/repos":
		result.BackURL = "/"
	}
	if strings.HasPrefix(path, "/manage/") {
		result.Subtitle = "Manage"
	}
	if data.Tab == "signin" {
		result.Title = "Sign in"
		result.Subtitle = data.Slug
	}
	return result
}

func mobileReturnPath(raw, base string) (string, bool) {
	if raw == "" || strings.IndexFunc(raw, func(r rune) bool { return r < 0x20 || r == '\\' }) >= 0 {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || u.Path == "/signin" || !strings.HasPrefix(u.Path, "/") {
		return "", false
	}
	decodedPath, err := url.PathUnescape(u.Path)
	if err != nil || strings.IndexFunc(decodedPath, func(r rune) bool { return r < 0x20 || r == '\\' }) >= 0 {
		return "", false
	}
	if base != "" && strings.HasPrefix(u.Path, "/r/") && !strings.HasPrefix(u.Path, strings.TrimSuffix(base, "/")+"/") {
		return "", false
	}
	return raw, true
}

func eventShortTitle(value any, fallback string) string {
	row := valueMap(value)
	if post := valueMap(row["post"]); post != nil {
		row = post
	}
	for _, key := range []string{"name", "filename", "title", "d"} {
		if title := strings.TrimSpace(plainString(row[key])); title != "" && !eventIDPattern.MatchString(title) {
			return shortHeaderText(title)
		}
	}
	if content := strings.TrimSpace(plainString(row["content"])); content != "" {
		return shortHeaderText(strings.Split(content, "\n")[0])
	}
	return fallback
}

func shortHeaderText(value string) string {
	value = strings.TrimSpace(value)
	if len([]rune(value)) > 64 {
		return string([]rune(value)[:61]) + "..."
	}
	return value
}

func mobileTitle(path string, data PageData) string {
	known := map[string]string{"/": "Home", "/home": "Home", "/search": "Search", "/social": "Social", "/chat": "Chat", "/files": "Files", "/file": "File", "/wiki": "Wiki", "/repos": "Repositories", "/repo": "Repository", "/approvals": "Approvals", "/profile": "Profile", "/account": "Account", "/manage": "Manage", "/manage/people": "People", "/manage/agents": "Agents", "/manage/moderation": "Moderation", "/manage/rules": "Rules", "/manage/identity": "Identity", "/manage/connect": "Connect", "/manage/owner": "Owner", "/manage/sync": "Sync", "/manage/data": "Data", "/manage/views": "Views", "/manage/health": "Health", "/sites": "Sites"}
	if title, ok := known[path]; ok {
		return title
	}
	if strings.HasPrefix(path, "/social/") {
		return "Social"
	}
	if strings.HasPrefix(path, "/wiki/") {
		return "Wiki"
	}
	if strings.HasPrefix(path, "/repos/") {
		return "Repositories"
	}
	if title := strings.TrimSuffix(data.Title, " | "+data.Slug); title != "" && !eventIDPattern.MatchString(title) {
		return title
	}
	return "Tiny"
}

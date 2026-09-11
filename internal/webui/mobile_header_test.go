package webui

import (
	"net/url"
	"strings"
	"testing"
)

func TestMobileHeaderGeneralRoutes(t *testing.T) {
	tests := []struct {
		path, title, wantTitle, wantSubtitle, wantBack string
	}{
		{"/", "tiny | relay", "Home", "tiny", ""},
		{"/social", "social | tiny", "Social", "tiny", "/"},
		{"/files", "Files | tiny", "Files", "tiny", "/"},
		{"/wiki", "Wiki | tiny", "Wiki", "tiny", "/"},
		{"/repos", "Repositories | tiny", "Repositories", "tiny", "/"},
		{"/manage/people", "people | tiny", "People", "Manage", "/"},
		{"/social/thread/abc", "Social | tiny", "Social", "tiny", "/social"},
		{"/file", "File | tiny", "File", "tiny", "/files"},
		{"/repo", "Repository | tiny", "Repository", "tiny", "/repos"},
		{"/profile", "Profile | tiny", "Profile", "tiny", "/account"},
		{"/sites", "sites | tiny", "Sites", "tiny", "/"},
	}
	for _, test := range tests {
		got := mobileHeader(PageData{Path: test.path, Title: test.title, Slug: "tiny"})
		if got.Title != test.wantTitle || got.Subtitle != test.wantSubtitle || got.BackURL != test.wantBack || got.BackLabel == "" && test.wantBack != "" {
			t.Errorf("mobileHeader(%q) = %+v, want title=%q subtitle=%q back=%q", test.path, got, test.wantTitle, test.wantSubtitle, test.wantBack)
		}
	}
}

func TestMobileHeaderRoomAndThread(t *testing.T) {
	room := map[string]any{"room": map[string]any{"id": "general", "name": "The <General> Room", "picture": "https://cdn.example/room.png"}}
	got := mobileHeader(PageData{Path: "/rooms/general", Tab: "room", Event: room, URL: "https://relay.example"})
	if got.Title != "The <General> Room" || got.Subtitle != "Chat" || got.BackURL != "/chat" || got.RoomID != "general" || got.Picture == "" {
		t.Fatalf("room header = %+v", got)
	}
	root := strings.Repeat("a", 64)
	thread := mobileHeader(PageData{Path: "/rooms/general/thread/" + root, Tab: "thread", Event: map[string]any{
		"room": map[string]any{"id": "general", "name": "General"},
		"root": map[string]any{"id": root},
	}})
	if thread.Title != "Thread" || thread.Subtitle != "General" || thread.BackURL != "/rooms/general#msg-"+root || thread.BackLabel != "Back to room" {
		t.Fatalf("thread header = %+v", thread)
	}
}

func TestMobileHeaderDirectAndHiddenData(t *testing.T) {
	peer := strings.Repeat("b", 64)
	got := mobileHeader(PageData{Path: "/chat/dm/" + peer, Tab: "direct", View: peer})
	if got.Title != "Direct chat" || got.Subtitle != "Private conversation" || got.PubKey != peer || got.BackURL != "/chat" {
		t.Fatalf("direct header = %+v", got)
	}
	private := mobileHeader(PageData{Path: "/rooms/secret", Tab: "room", Private: true, Event: map[string]any{"room": map[string]any{"name": "Hidden", "picture": "https://secret.example/p.png"}}})
	if private.Title != "Sign in" || private.Subtitle != "" || private.Picture != "" || private.RoomID != "" {
		t.Fatalf("private header leaked data: %+v", private)
	}
	privateDirect := mobileHeader(PageData{Path: "/chat/dm/" + peer, Tab: "direct", View: peer, Private: true, Slug: "tiny", Event: map[string]any{"pubkey": peer}})
	if privateDirect.Title != "Sign in" || privateDirect.PubKey != "" || privateDirect.RoomID != "" || privateDirect.Picture != "" {
		t.Fatalf("private direct header leaked data: %+v", privateDirect)
	}
	err := mobileHeader(PageData{Path: "/rooms/secret", Tab: "room", Error: "denied", Event: map[string]any{"room": map[string]any{"name": "Hidden"}}})
	if err.Title != "Chat" || err.RoomID != "" || err.BackURL != "/chat" || err.BackLabel != "Back to chats" {
		t.Fatalf("error header leaked data: %+v", err)
	}
	signin := mobileHeader(PageData{Path: "/manage/people", Tab: "signin", Title: "People | tiny", Slug: "tiny"})
	if signin.Title != "Sign in" || signin.Subtitle != "tiny" {
		t.Fatalf("signin header = %+v", signin)
	}
}

func TestMobileHeaderUsesDeepLinkContext(t *testing.T) {
	article := mobileHeader(PageData{Path: "/social/" + strings.Repeat("a", 64), Tab: "social-thread", Slug: "tiny", Event: map[string]any{
		"post": map[string]any{"title": "A useful article"},
	}})
	if article.Title != "A useful article" || article.Subtitle != "Social" || article.BackURL != "/social" || article.BackLabel != "Back to social" {
		t.Fatalf("article header = %+v", article)
	}
	file := mobileHeader(PageData{Path: "/file", Tab: "file", Slug: "tiny", Event: map[string]any{"name": "photo.mp4"}})
	if file.Title != "photo.mp4" || file.Subtitle != "Files" || file.BackURL != "/files" || file.BackLabel != "Back to files" {
		t.Fatalf("file header = %+v", file)
	}
	repo := mobileHeader(PageData{Path: "/repo", Tab: "repo", Slug: "tiny", Query: url.Values{"repo": []string{"seedmark"}, "view": []string{"issues"}}})
	if repo.Title != "seedmark" || repo.Subtitle != "issues" || repo.BackURL != "/repos" || repo.BackLabel != "Back to repositories" {
		t.Fatalf("repo header = %+v", repo)
	}
	wiki := mobileHeader(PageData{Path: "/wiki/release-notes", Tab: "wikipage", Slug: "tiny", Query: url.Values{"d": []string{"release-notes"}}, Event: map[string]any{"title": "Release notes"}})
	if wiki.Title != "Release notes" || wiki.Subtitle != "Wiki" || wiki.BackURL != "/wiki" {
		t.Fatalf("wiki header = %+v", wiki)
	}
	sites := mobileHeader(PageData{Path: "/files", Tab: "files", Slug: "tiny", Query: url.Values{"view": []string{"sites"}}})
	if sites.Title != "Sites" || sites.Subtitle != "Files" || sites.BackURL != "/" {
		t.Fatalf("sites header = %+v", sites)
	}
}

func TestMobileHeaderManagementParent(t *testing.T) {
	got := mobileHeader(PageData{Path: "/manage/agents", Tab: "agents", Slug: "tiny"})
	if got.BackURL != "/manage/people" || got.BackLabel != "Back to manage" {
		t.Fatalf("management header = %+v", got)
	}
}

func TestMobileHeaderSigninReturnValidation(t *testing.T) {
	valid := mobileHeader(PageData{Tab: "signin", Slug: "tiny", Base: "/r/demo", Query: url.Values{"next": []string{"/r/demo/social?q=hello"}}})
	if valid.BackURL != "/r/demo/social?q=hello" {
		t.Fatalf("valid signin return = %+v", valid)
	}
	for _, next := range []string{"/\\evil.test", "//evil.test", "/r/other/chat", "/r/demo/%5Cevil", "/signin"} {
		got := mobileHeader(PageData{Tab: "signin", Slug: "tiny", Base: "/r/demo", Query: url.Values{"next": []string{next}}})
		if got.BackURL != "" {
			t.Errorf("unsafe signin return %q accepted as %+v", next, got)
		}
	}
	connected := mobileHeader(PageData{Tab: "signin", Actor: strings.Repeat("a", 64), Slug: "tiny", Query: url.Values{"connect": []string{"1"}}})
	if connected.Title != "Connect signer" {
		t.Fatalf("connected signin header = %+v", connected)
	}
	account := mobileHeader(PageData{Tab: "signin", Actor: strings.Repeat("a", 64), Slug: "tiny"})
	if account.Title != "Account" {
		t.Fatalf("account signin header = %+v", account)
	}
}

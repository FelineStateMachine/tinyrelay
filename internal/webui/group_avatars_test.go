package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

type pictureRoomsBackend struct{ *roomsBackend }

func (b *pictureRoomsBackend) Query(ctx context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	result, err := b.roomsBackend.Query(ctx, method, params, actor)
	if err != nil {
		return result, err
	}
	page, ok := result.(map[string]any)
	if !ok {
		return result, nil
	}
	if method == "browserooms" {
		for _, item := range page["items"].([]any) {
			item.(map[string]any)["picture"] = "https://cdn.example/general.png"
		}
	} else if room, ok := page["room"].(map[string]any); ok {
		room["picture"] = "https://cdn.example/general.png"
	}
	return page, nil
}

func TestRoomSurfacesRenderStableSeedmarkFallbackAvatars(t *testing.T) {
	app, _ := roomsApp(t, roomOwner)
	for _, path := range []string{"/chat", "/rooms/general", "/rooms/general/thread/" + roomThread} {
		body := roomsPage(t, app, path)
		if !strings.Contains(body, `fallback="/avatars/v1/`) {
			t.Fatalf("%s has no Seedmark room fallback: %s", path, body)
		}
		if strings.Count(body, "<room-avatar") == 0 {
			t.Fatalf("%s has no room avatar element", path)
		}
	}
}

func TestRoomAvatarSeedAndTenantScoping(t *testing.T) {
	first := roomAvatarURL("https://relay.example", "general")
	if first == "" || first != roomAvatarURL("https://relay.example", "general") {
		t.Fatalf("room avatar URL is not stable: %q", first)
	}
	if first == roomAvatarURL("https://relay.example", "build") {
		t.Fatal("different room IDs share an avatar URL")
	}
	if scoped := injectBase(`<room-avatar fallback="`+first+`"><img src="`+first+`"></room-avatar>`, "/r/alice"); !strings.Contains(scoped, `fallback="/r/alice`+first+`"`) || !strings.Contains(scoped, `src="/r/alice`+first+`"`) {
		t.Fatalf("tenant prefix missed room avatar URL: %s", scoped)
	}
}

func TestRoomAvatarUsesValidatedPictureForSSR(t *testing.T) {
	base := &roomsBackend{fakeBackend: fakeBackend{policy: policy.Defaults(roomOwner)}}
	app, err := New(&pictureRoomsBackend{roomsBackend: base}, Options{Actor: func(*http.Request) (string, error) { return roomOwner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/rooms/general", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `picture="https://cdn.example/general.png"`) || !strings.Contains(body, `<img src="https://cdn.example/general.png"`) {
		t.Fatalf("validated room picture was not preferred in SSR: %s", body)
	}
}

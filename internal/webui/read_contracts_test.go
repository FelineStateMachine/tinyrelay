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

type typedRoomsBackend struct{ fakeBackend }

func (typedRoomsBackend) Query(context.Context, string, []json.RawMessage, string) (any, error) {
	panic("typed room reads must not use Backend.Query")
}

func (typedRoomsBackend) ListRooms(context.Context, string, string, int) (RoomList, error) {
	return RoomList{Rooms: []RoomSummary{{ID: "build", Name: "Build", Access: "open"}}}, nil
}

func (typedRoomsBackend) ReadRoom(context.Context, string, string, string, int) (RoomPage, error) {
	return RoomPage{Room: RoomSummary{ID: "build", Name: "Build"}}, nil
}

func (typedRoomsBackend) ReadThread(context.Context, string, string, string, string, int) (RoomPage, error) {
	return RoomPage{Room: RoomSummary{ID: "build", Name: "Build"}}, nil
}

func TestTypedRoomsReaderAvoidsLegacyQuery(t *testing.T) {
	backend := typedRoomsBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	app, err := New(&backend, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/rooms/build"} {
		response := httptest.NewRecorder()
		app.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, response.Code)
		}
		if !strings.Contains(response.Body.String(), "Build") {
			t.Fatalf("%s: typed room result missing from page", path)
		}
	}
}

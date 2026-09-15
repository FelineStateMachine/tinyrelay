package tinyclient

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

type remoteTypedBackend struct{ typedRoomsBackend }

func (remoteTypedBackend) Query(context.Context, string, []json.RawMessage, string) (any, error) {
	return nil, nil
}
func (remoteTypedBackend) ReadChatActivity(context.Context, string, string, string) (any, error) {
	return map[string]any{"jobs": []any{map[string]any{"id": strings.Repeat("b", 64), "content": "A visible agent task", "state": "open", "status": "queued", "created_at": 1}}}, nil
}

func TestRemotePreservesTypedRoomAndActivityRendering(t *testing.T) {
	backend := remoteTypedBackend{typedRoomsBackend{fakeBackend: fakeBackend{policy: DefaultPolicy(strings.Repeat("a", 64))}}}
	app, err := New(&backend, Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(app)
	defer upstream.Close()
	remote, err := NewRemote(RemoteOptions{BackendURL: upstream.URL, PublicURL: backend.URL()})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/chat", "/rooms/build", "/rooms/build/thread/" + strings.Repeat("b", 64), "/chat/activity?room=build"} {
		want, got := httptest.NewRecorder(), httptest.NewRecorder()
		app.ServeHTTP(want, httptest.NewRequest("GET", path, nil))
		remote.ServeHTTP(got, httptest.NewRequest("GET", backend.URL()+path, nil))
		if want.Code != got.Code || want.Body.String() != got.Body.String() {
			t.Fatalf("typed parity %s: got status %d want %d\n%s", path, got.Code, want.Code, got.Body.String())
		}
	}
}

package tinyclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type actorReadBackend struct{ actorFeedBackend }

func (b *actorReadBackend) ListRooms(_ context.Context, actor, _ string, _ int) (RoomList, error) {
	return RoomList{Rooms: []RoomSummary{{ID: "public-article", Name: actor}}}, nil
}
func (b *actorReadBackend) ReadRoom(_ context.Context, actor, _, _ string, _ int) (RoomPage, error) {
	return RoomPage{Room: RoomSummary{ID: "public-article", Name: actor}}, nil
}
func (b *actorReadBackend) ReadThread(_ context.Context, actor, _, _, _ string, _ int) (RoomPage, error) {
	return RoomPage{Room: RoomSummary{ID: "public-article", Name: actor}}, nil
}
func (b *actorReadBackend) ReadChatActivity(_ context.Context, actor, _, _ string) (any, error) {
	return map[string]string{"id": "public-article", "actor": actor}, nil
}

func TestRemoteReadsPreserveRequestedActor(t *testing.T) {
	owner, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	backend := &actorReadBackend{actorFeedBackend{fakeBackend{policy: DefaultPolicy(owner)}}}
	app, err := New(backend, Options{Actor: func(r *http.Request) (string, error) {
		if cookie, err := r.Cookie("tiny_session"); err == nil && cookie.Value == "valid-session" {
			return owner, nil
		}
		return "", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		app.BackendHandler().ServeHTTP(w, r)
	}))
	defer upstream.Close()
	handler, err := NewRemote(RemoteOptions{BackendURL: upstream.URL, PublicURL: backend.URL()})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, cookie, actor string
		denied              bool
	}{
		{name: "authenticated", cookie: "tiny_session=valid-session", actor: owner},
		{name: "anonymous read with session", cookie: "tiny_session=valid-session"},
		{name: "mismatched actor", cookie: "tiny_session=valid-session", actor: other, denied: true},
		{name: "anonymous"},
		{name: "forged actor without session", actor: owner, denied: true},
		{name: "forged actor with invalid session", cookie: "tiny_session=invalid", actor: owner, denied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &requestBackend{remote: handler.(*remoteClient), cookie: tc.cookie}
			if err := b.remote.read(context.Background(), b.cookie, nil, &b.snapshot); err != nil {
				t.Fatal(err)
			}
			for name, read := range map[string]func() (any, error){
				"query": func() (any, error) { return b.Query(context.Background(), "queryevents", nil, tc.actor) },
				"rooms": func() (any, error) { return (&remoteRooms{b}).ListRooms(context.Background(), tc.actor, "", 10) },
				"room": func() (any, error) {
					return (&remoteRooms{b}).ReadRoom(context.Background(), tc.actor, "build", "", 10)
				},
				"thread": func() (any, error) {
					return (&remoteRooms{b}).ReadThread(context.Background(), tc.actor, "build", "root", "", 10)
				},
				"activity": func() (any, error) { return b.ReadChatActivity(context.Background(), tc.actor, "build", "root") },
			} {
				t.Run(name, func(t *testing.T) {
					before := reads.Load()
					result, err := read()
					if tc.denied {
						if err == nil || reads.Load() != before {
							t.Fatalf("mismatched actor must fail before forwarding: err=%v upstream reads=%d", err, reads.Load()-before)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					raw, err := json.Marshal(result)
					if err != nil {
						t.Fatal(err)
					}
					private := owner
					if name == "query" {
						private = "session-only-article"
					}
					if !strings.Contains(string(raw), "public-article") || strings.Contains(string(raw), private) != (tc.actor == owner) {
						t.Fatalf("read used wrong actor: %s", raw)
					}
				})
			}
		})
	}
}

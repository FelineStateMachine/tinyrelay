package tinyclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

func (a *App) typedBackendRead(w http.ResponseWriter, r *http.Request, actor string) {
	if a.backend.Policy().Features.Grasp08 {
		if err := a.privateReadAllowed(r, actor); err != nil {
			http.Error(w, err.Error(), 403)
			return
		}
	}
	q := r.URL.Query()
	var result any
	var err error
	switch q.Get("op") {
	case "rooms", "room", "thread":
		reader, ok := a.backend.(RoomsReader)
		if !ok {
			http.Error(w, "typed room reads unavailable", 501)
			return
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		switch q.Get("op") {
		case "rooms":
			result, err = reader.ListRooms(r.Context(), actor, q.Get("cursor"), limit)
		case "room":
			result, err = reader.ReadRoom(r.Context(), actor, q.Get("room"), q.Get("cursor"), limit)
		case "thread":
			result, err = reader.ReadThread(r.Context(), actor, q.Get("room"), q.Get("root"), q.Get("cursor"), limit)
		}
	case "activity":
		reader, ok := a.backend.(ChatActivityReader)
		if !ok {
			http.Error(w, "chat activity unavailable", 501)
			return
		}
		result, err = reader.ReadChatActivity(r.Context(), actor, q.Get("room"), q.Get("root"))
	default:
		err = errors.New("unsupported client read")
	}
	if err != nil {
		http.Error(w, err.Error(), 403)
		return
	}
	writeJSON(w, 200, result)
}

type remoteRooms struct{ *requestBackend }

func (b *remoteRooms) ListRooms(ctx context.Context, actor, cursor string, limit int) (RoomList, error) {
	var result RoomList
	err := b.readAsActor(ctx, actor, url.Values{"op": {"rooms"}, "cursor": {cursor}, "limit": {strconv.Itoa(limit)}}, &result)
	return result, err
}
func (b *remoteRooms) ReadRoom(ctx context.Context, actor, room, cursor string, limit int) (RoomPage, error) {
	var result RoomPage
	err := b.readAsActor(ctx, actor, url.Values{"op": {"room"}, "room": {room}, "cursor": {cursor}, "limit": {strconv.Itoa(limit)}}, &result)
	return result, err
}
func (b *remoteRooms) ReadThread(ctx context.Context, actor, room, root, cursor string, limit int) (RoomPage, error) {
	var result RoomPage
	err := b.readAsActor(ctx, actor, url.Values{"op": {"thread"}, "room": {room}, "root": {root}, "cursor": {cursor}, "limit": {strconv.Itoa(limit)}}, &result)
	return result, err
}
func (b *requestBackend) ReadChatActivity(ctx context.Context, actor, room, root string) (any, error) {
	if !b.snapshot.Activity {
		return nil, nil
	}
	var result any
	err := b.readAsActor(ctx, actor, url.Values{"op": {"activity"}, "room": {room}, "root": {root}}, &result)
	return result, err
}

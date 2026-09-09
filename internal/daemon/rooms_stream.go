package daemon

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

const (
	roomStreamKeepAlive = 25 * time.Second
	roomStreamBuffer    = 64
)

var roomStreamPath = regexp.MustCompile(`^/rooms/([a-z0-9_-]{1,64})/stream$`)

func isRoomStreamPath(path string) bool { return roomStreamPath.MatchString(path) }

// roomStreamHTTP serves GET /rooms/<id>/stream as server-sent events. Each
// event the viewer may see in the room arrives as "event: message" with the
// JSON event as data; a comment keeps the connection alive. The viewer is
// identified by the browser session cookie or a NIP-98 signature.
func (t *Tenant) roomStreamHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, finish := t.app.telemetry.Start(r.Context(), "rooms")
	outcome := "error"
	defer func() { finish(outcome) }()
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		outcome = "invalid"
		return
	}
	id := roomStreamPath.FindStringSubmatch(r.URL.Path)[1]
	if origin := r.Header.Get("Origin"); origin != "" && r.Header.Get("Authorization") == "" {
		base, err := url.Parse(t.requestURL(r))
		if err != nil || origin != base.Scheme+"://"+base.Host {
			http.Error(w, "auth-required: sign this request", http.StatusUnauthorized)
			outcome = "unauthorized"
			return
		}
	}
	actor, err := t.resolveUIActor(r)
	if err != nil {
		browseHTTPError(w, err)
		outcome = "unauthorized"
		return
	}
	// Hold an operation only while checking access. A stream may stay open
	// for hours and must not keep maintenance waiting; it ends instead when
	// maintenance or shutdown begins.
	opCtx, done, err := t.beginOperation(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		outcome = "busy"
		return
	}
	err = t.browseRead(opCtx, actor)
	if err == nil {
		_, _, err = t.roomFor(opCtx, actor, id)
	}
	done()
	if err != nil {
		browseHTTPError(w, err)
		outcome = "unauthorized"
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	session := browseSession(t, actor)
	events := make(chan event.Event, roomStreamBuffer)
	overflow := make(chan struct{}, 1)
	stop := t.router.Listen(func(e event.Event) {
		if e.Kind == event.KIND_MARMOT_GROUP || event.Tag(e, "h") != id {
			return
		}
		select {
		case events <- e:
		default:
			select {
			case overflow <- struct{}{}:
			default:
			}
		}
	})
	defer stop()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(": room " + id + "\n\n")); err != nil {
		return
	}
	flusher.Flush()
	keepAlive := time.NewTicker(roomStreamKeepAlive)
	defer keepAlive.Stop()
	delivered := 0
	defer func() {
		t.app.telemetry.Logger().Debug("room stream closed", "events", delivered, "outcome", outcome)
	}()
	paused := t.maintenance.watch()
	for {
		select {
		case <-r.Context().Done():
			outcome = "closed"
			return
		case <-t.workCtx.Done():
			outcome = "closed"
			return
		case <-paused:
			if t.maintenance.blocked() {
				outcome = "closed"
				return
			}
			paused = t.maintenance.watch()
		case <-overflow:
			outcome = "busy"
			return
		case <-keepAlive.C:
			if _, err := w.Write([]byte(": keep-alive\n\n")); err != nil {
				outcome = "closed"
				return
			}
			flusher.Flush()
		case e := <-events:
			if !t.gate.CanSee(r.Context(), e, session, nil) {
				continue
			}
			raw, err := json.Marshal(e)
			if err != nil {
				continue
			}
			var frame strings.Builder
			frame.WriteString("event: message\ndata: ")
			frame.Write(raw)
			frame.WriteString("\n\n")
			if _, err := w.Write([]byte(frame.String())); err != nil {
				outcome = "closed"
				return
			}
			flusher.Flush()
			delivered++
		}
	}
}

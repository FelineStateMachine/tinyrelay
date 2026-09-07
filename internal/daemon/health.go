package daemon

import (
	"context"
	"net/http"
	"time"
)

const healthProbeTimeout = 500 * time.Millisecond

func (a *App) healthHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		if a.isClosed() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "shutting_down"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
		return
	}
	if r.URL.Path == "/readyz" {
		if err := a.ready(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	}
	http.NotFound(w, r)
}

func (a *App) ready(parent context.Context) error {
	if a.isClosed() {
		return ErrTenantClosing
	}
	a.lifecycle.mu.Lock()
	started := a.lifecycle.started
	a.lifecycle.mu.Unlock()
	if !started {
		return ErrMaintenance
	}
	ctx, cancel := context.WithTimeout(parent, healthProbeTimeout)
	defer cancel()
	_, err := a.catalog.List(ctx)
	return err
}

func (a *App) isClosed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closed
}

func (t *Tenant) healthHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, done, err := t.beginOperation(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	defer done()
	probeCtx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()
	if err := t.store.DB().PingContext(probeCtx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

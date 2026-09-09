package daemon

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrMaintenance     = errors.New("retryable: tenant maintenance in progress")
	ErrMaintenanceBusy = errors.New("retryable: tenant maintenance is already active")
	ErrTenantClosing   = errors.New("tenant is closing")
)

type maintenanceGate struct {
	mu          sync.Mutex
	active      int
	maintenance bool
	closing     bool
	changed     chan struct{}
}

type maintenanceContextKey struct{}

type maintenanceContext struct {
	gate        *maintenanceGate
	maintenance bool
}

func (t *Tenant) beginOperation(ctx context.Context) (context.Context, func(), error) {
	return t.maintenance.beginOperation(ctx)
}
func (t *Tenant) beginMaintenance(ctx context.Context) (context.Context, func(), error) {
	return t.maintenance.beginMaintenance(ctx)
}

func (g *maintenanceGate) waitIdle(ctx context.Context) error {
	remaining := 0
	ownMaintenance := false
	if marker, ok := ctx.Value(maintenanceContextKey{}).(maintenanceContext); ok && marker.gate == g {
		ownMaintenance = marker.maintenance
		if !marker.maintenance {
			remaining = 1
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.active > remaining || g.maintenance && !ownMaintenance {
		if g.changed == nil {
			g.changed = make(chan struct{})
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			g.mu.Lock()
			return ctx.Err()
		case <-changed:
		}
		g.mu.Lock()
	}
	return nil
}

func (g *maintenanceGate) beginOperation(ctx context.Context) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if marker, ok := ctx.Value(maintenanceContextKey{}).(maintenanceContext); ok && marker.gate == g {
		return ctx, func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		return ctx, nil, ErrTenantClosing
	}
	if g.maintenance {
		g.mu.Unlock()
		return ctx, nil, ErrMaintenance
	}
	g.active++
	g.mu.Unlock()
	marked := context.WithValue(ctx, maintenanceContextKey{}, maintenanceContext{gate: g})
	return marked, g.endOperation(), nil
}

func (g *maintenanceGate) beginMaintenance(ctx context.Context) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if marker, ok := ctx.Value(maintenanceContextKey{}).(maintenanceContext); ok && marker.gate == g {
		if marker.maintenance {
			return ctx, func() {}, nil
		}
		return ctx, nil, ErrMaintenanceBusy
	}
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		return ctx, nil, ErrTenantClosing
	}
	if g.maintenance {
		g.mu.Unlock()
		return ctx, nil, ErrMaintenanceBusy
	}
	g.maintenance = true
	g.signalLocked()
	for g.active > 0 {
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			g.mu.Lock()
			g.maintenance = false
			g.signalLocked()
			g.mu.Unlock()
			return ctx, nil, ctx.Err()
		case <-changed:
		}
		g.mu.Lock()
	}
	g.mu.Unlock()
	marked := context.WithValue(ctx, maintenanceContextKey{}, maintenanceContext{gate: g, maintenance: true})
	return marked, g.endMaintenance(), nil
}

func (g *maintenanceGate) markClosing() {
	g.mu.Lock()
	g.closing = true
	g.signalLocked()
	g.mu.Unlock()
}

func (g *maintenanceGate) endOperation() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if g.active > 0 {
				g.active--
			}
			g.signalLocked()
			g.mu.Unlock()
		})
	}
}

func (g *maintenanceGate) endMaintenance() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.maintenance = false
			g.signalLocked()
			g.mu.Unlock()
		})
	}
}

// watch returns a channel that closes on the next gate change, so a
// long-lived reader can stop when maintenance or shutdown begins.
func (g *maintenanceGate) watch() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.changed == nil {
		g.changed = make(chan struct{})
	}
	return g.changed
}

// blocked reports whether new operations are being refused.
func (g *maintenanceGate) blocked() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maintenance || g.closing
}

func (g *maintenanceGate) signalLocked() {
	if g.changed != nil {
		close(g.changed)
	}
	g.changed = make(chan struct{})
}

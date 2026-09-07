package daemon

import (
	"context"
	"errors"

	"github.com/FelineStateMachine/tinyrelay/internal/catalog"
)

// deleteTenant stops the live services before erasing the catalog row and
// tenant directory. It removes the app map entry before waiting so a request
// arriving concurrently cannot acquire a second reference to the tenant.
func (a *App) deleteTenant(ctx context.Context, id string) error {
	a.mu.Lock()
	t := a.tenants[id]
	if t == nil {
		a.mu.Unlock()
		return catalog.ErrNotFound
	}
	if err := a.catalog.Disable(ctx, id); err != nil && !errors.Is(err, catalog.ErrInvalidTransition) {
		a.mu.Unlock()
		return err
	}
	delete(a.tenants, id)
	a.mu.Unlock()
	if err := t.Close(ctx); err != nil {
		return err
	}
	return a.catalog.Delete(ctx, id)
}

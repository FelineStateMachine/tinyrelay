package daemon

import (
	"context"
	"errors"
	"time"
)

// Network synchronization has its own lifecycle so an unavailable peer does
// not delay local retention, view publication or upload cleanup.
func (t *Tenant) runGitHistory(ctx context.Context) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if t.gitLiveEnabled() {
			opCtx, done, err := t.beginOperation(ctx)
			if err == nil {
				passCtx, cancel := context.WithTimeout(opCtx, 5*time.Minute)
				err = t.git.GRASPService().Tick(passCtx)
				cancel()
				done()
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				t.app.telemetry.Logger().Error("Git synchronization failed", "tenant", t.meta.Name, "error", err)
			}
		}
		timer.Reset(time.Minute)
	}
}

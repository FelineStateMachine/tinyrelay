package daemon

import (
	"context"
	"errors"
	"time"
)

func (t *Tenant) scheduleViews() {
	select {
	case t.schedulerWake <- struct{}{}:
	default:
	}
}

// The timer follows view deadlines and wakes when writes mark a view dirty.
// Slow maintenance keeps its own cadence; idle tenants do not poll view SQL.
func (t *Tenant) runScheduler() {
	next := time.Now().Add(time.Second)
	nextMaintenance := next
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()
	for {
		wake := false
		select {
		case <-t.workCtx.Done():
			return
		case <-t.schedulerWake:
			wake = true
		case <-timer.C:
		}
		opCtx, done, err := t.beginOperation(t.workCtx)
		if err != nil {
			next = time.Now().Add(time.Second)
			timer.Reset(time.Second)
			continue
		}
		ctx, cancel := context.WithTimeout(opCtx, 30*time.Second)
		now := time.Now()
		report := func(name string, err error) {
			if err != nil && !errors.Is(err, context.Canceled) {
				t.app.telemetry.Logger().Error("tenant maintenance failed", "tenant", t.meta.Name, "operation", name, "error", err)
			}
		}
		if !wake {
			report("records", t.records.Tick(ctx, now.Unix()))
			if !now.Before(nextMaintenance) {
				if t.blobs != nil {
					report("partial uploads", t.blobs.CleanupMultipart(ctx))
				}
				report("expired uploads", t.sweepExpiredBlobs(ctx, now.Unix()))
				report("retention", t.sweep(ctx, now.Unix()))
				report("restore-state", t.sweepReplicationState(ctx, now.Unix()))
				nextMaintenance = now.Add(time.Minute)
			}
			next = nextMaintenance
		}
		due, dueErr := t.records.NextDue(ctx, time.Now().Unix())
		report("view schedule", dueErr)
		if dueErr == nil && due > 0 {
			viewAt := time.Unix(due, 0)
			if viewAt.Before(next) {
				next = viewAt
			}
		}
		cancel()
		done()
		delay := time.Until(next)
		if delay < time.Second {
			delay = time.Second
			next = time.Now().Add(delay)
		}
		timer.Reset(delay)
	}
}

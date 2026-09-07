package daemon

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMaintenanceWaitsForPublishAndResumesOperations(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()

	// A publish holds the normal operation admission slot while the backup
	// requests exclusive maintenance.
	publishCtx, releasePublish, err := tenant.beginOperation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if publishCtx == nil {
		t.Fatal("publish context is nil")
	}
	maintenanceReady := make(chan struct{})
	maintenanceDone := make(chan error, 1)
	go func() {
		close(maintenanceReady)
		_, release, err := tenant.beginMaintenance(ctx)
		if err != nil {
			maintenanceDone <- err
			return
		}
		maintenanceDone <- nil
		release()
	}()
	<-maintenanceReady

	select {
	case err := <-maintenanceDone:
		t.Fatalf("maintenance passed active publish: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-maintenanceDone:
		t.Fatal("maintenance completed before active publish was released")
	default:
	}
	if _, release, err := tenant.beginOperation(ctx); !errors.Is(err, ErrMaintenance) {
		if release != nil {
			release()
		}
		t.Fatalf("operation during maintenance = %v, want %v", err, ErrMaintenance)
	}

	releasePublish()
	select {
	case err := <-maintenanceDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance did not start after publish completed")
	}

	// The maintenance release is performed by the goroutine after it signals
	// completion, so the next operation should be admitted shortly afterward.
	deadline := time.Now().Add(time.Second)
	for {
		_, release, err := tenant.beginOperation(ctx)
		if err == nil {
			release()
			return
		}
		if !errors.Is(err, ErrMaintenance) {
			t.Fatalf("operation after maintenance = %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("normal operations did not resume")
		}
		time.Sleep(time.Millisecond)
	}
}

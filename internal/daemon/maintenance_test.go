package daemon

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMaintenanceDrainsAndRejectsNewOperations(t *testing.T) {
	var gate maintenanceGate
	_, finish, err := gate.beginOperation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		close(started)
		_, release, beginErr := gate.beginMaintenance(context.Background())
		if beginErr == nil {
			release()
		}
		finished <- beginErr
	}()
	<-started
	select {
	case <-finished:
		t.Fatal("maintenance completed before active operation drained")
	case <-time.After(20 * time.Millisecond):
	}
	if _, _, err := gate.beginOperation(context.Background()); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("new operation error = %v", err)
	}
	finish()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance did not acquire after drain")
	}
}

func TestMaintenanceCancellationClearsAdmissionFlag(t *testing.T) {
	var gate maintenanceGate
	_, finish, err := gate.beginOperation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, beginErr := gate.beginMaintenance(ctx)
		result <- beginErr
	}()
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("maintenance cancellation error = %v", err)
	}
	finish()
	_, release, err := gate.beginOperation(context.Background())
	if err != nil {
		t.Fatalf("operation admission remained blocked: %v", err)
	}
	release()
}

func TestMaintenanceIsExclusiveAndNestedMarkersDoNotDeadlock(t *testing.T) {
	var gate maintenanceGate
	maintenanceCtx, release, err := gate.beginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, nestedRelease, err := gate.beginMaintenance(maintenanceCtx); err != nil {
		t.Fatal(err)
	} else {
		nestedRelease()
	}
	if _, _, err := gate.beginMaintenance(context.Background()); !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("concurrent maintenance error = %v", err)
	}
	if _, nestedRelease, err := gate.beginOperation(maintenanceCtx); err != nil {
		t.Fatal(err)
	} else {
		nestedRelease()
	}
}

func TestMaintenanceOperationMarkerAllowsNestedCallbacks(t *testing.T) {
	var gate maintenanceGate
	ctx, release, err := gate.beginOperation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	nested, nestedRelease, err := gate.beginOperation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	nestedRelease()
	if nested != ctx {
		t.Fatal("nested operation did not preserve context marker")
	}
}

func TestClosingPermanentlyRejectsOperationsAndMaintenance(t *testing.T) {
	var gate maintenanceGate
	gate.markClosing()
	if _, _, err := gate.beginOperation(context.Background()); !errors.Is(err, ErrTenantClosing) {
		t.Fatalf("closing operation error = %v", err)
	}
	if _, _, err := gate.beginMaintenance(context.Background()); !errors.Is(err, ErrTenantClosing) {
		t.Fatalf("closing maintenance error = %v", err)
	}
}

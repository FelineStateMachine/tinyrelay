package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/catalog"
)

// ProcessLock is held for the lifetime of a serving process. Catalog-only CLI
// commands remain usable while the server owns the directory.
type ProcessLock struct{ file *os.File }

type lifecycleState struct {
	mu          sync.Mutex
	started     bool
	processLock *ProcessLock
	baseURL     string
	cancel      context.CancelFunc
	done        chan struct{}
}

func AcquireProcessLock(dataDir string) (*ProcessLock, error) {
	if dataDir == "" {
		return nil, errors.New("daemon: data directory is required")
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dataDir, ".tiny.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("daemon: data directory is already served or cannot be locked: %w", err)
	}
	return &ProcessLock{file: file}, nil
}

func (l *ProcessLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	return errors.Join(syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN), l.file.Close())
}

// Start eagerly opens permanent ready tenants, so jobs resume after restart
// without waiting for the first browser or websocket request.
func (a *App) Start(ctx context.Context, baseURL string) error {
	a.lifecycle.mu.Lock()
	defer a.lifecycle.mu.Unlock()
	if a.lifecycle.started {
		return nil
	}
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed {
		return errors.New("daemon is shutting down")
	}
	lock, err := AcquireProcessLock(a.cfg.DataDir)
	if err != nil {
		return err
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	if err := a.reconcileTenants(ctx, baseURL); err != nil {
		return errors.Join(err, lock.Close())
	}
	a.lifecycle.processLock = lock
	a.lifecycle.started = true
	a.lifecycle.baseURL = baseURL
	runCtx, cancel := context.WithCancel(context.Background())
	a.lifecycle.cancel = cancel
	done := make(chan struct{})
	a.lifecycle.done = done
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if err := a.reconcileTenants(runCtx, baseURL); err != nil && runCtx.Err() == nil {
					a.telemetry.Logger().Error("reconcile tenants", "error", err)
				}
			}
		}
	}()
	return nil
}

func (a *App) stopLifecycle() error {
	a.lifecycle.mu.Lock()
	if !a.lifecycle.started {
		a.lifecycle.mu.Unlock()
		return nil
	}
	a.lifecycle.started = false
	done := a.lifecycle.done
	a.lifecycle.cancel()
	a.lifecycle.mu.Unlock()
	<-done
	return nil
}

func (a *App) releaseProcessLock() error {
	a.lifecycle.mu.Lock()
	defer a.lifecycle.mu.Unlock()
	lock := a.lifecycle.processLock
	a.lifecycle.processLock = nil
	return lock.Close()
}

func (a *App) startCreatedTenant(ctx context.Context, meta catalog.Tenant) error {
	a.lifecycle.mu.Lock()
	base, started := a.lifecycle.baseURL, a.lifecycle.started
	a.lifecycle.mu.Unlock()
	if !started || meta.Status != catalog.StatusReady {
		return nil
	}
	if meta.Name != a.cfg.DefaultTenant {
		base += "/r/" + meta.Name
	}
	_, err := a.tenant(ctx, meta, base)
	return err
}

func (a *App) reconcileTenants(ctx context.Context, base string) error {
	metas, err := a.catalog.List(ctx)
	if err != nil {
		return err
	}
	ready := make(map[string]bool, len(metas))
	for _, meta := range metas {
		ready[meta.ID] = meta.Status == catalog.StatusReady
	}
	var stopped []*Tenant
	a.mu.Lock()
	for id, tenant := range a.tenants {
		if !ready[id] {
			delete(a.tenants, id)
			stopped = append(stopped, tenant)
		}
	}
	a.mu.Unlock()
	var errs []error
	for _, tenant := range stopped {
		errs = append(errs, tenant.Close(ctx))
	}
	for _, meta := range metas {
		if !ready[meta.ID] {
			continue
		}
		u := base
		if meta.Name != a.cfg.DefaultTenant {
			u += "/r/" + meta.Name
		}
		if _, err := a.tenant(ctx, meta, u); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

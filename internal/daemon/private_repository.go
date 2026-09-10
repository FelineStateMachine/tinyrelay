package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// validatePrivateRepositoryPlacement prevents a private GRASP repository from
// being admitted to an open tenant. State events inherit the marker from their
// stored announcement, including when the announcement predates this guard.
func (t *Tenant) validatePrivateRepositoryPlacement(ctx context.Context, e event.Event) error {
	if e.Kind != event.KIND_REPO && e.Kind != event.KIND_REPO_STATE {
		return nil
	}
	private, err := privateRepositoryStored(ctx, t.store, e)
	if err != nil {
		return fmt.Errorf("check private repository placement: %w", err)
	}
	if !private {
		return nil
	}
	p := t.Policy()
	if !p.Features.Grasp08 || p.Reads != "members" {
		return errors.New("blocked: private repositories require a GRASP-08 private tenant")
	}
	return nil
}

func hasLegacyPrivateRepository(ctx context.Context, store *storage.Store) (bool, error) {
	if store == nil {
		return false, errors.New("private repository scan: missing store")
	}
	return store.HasPrivateRepositories(ctx)
}

func privateRepositoryStored(ctx context.Context, store *storage.Store, e event.Event) (bool, error) {
	return policy.PrivateRepository(ctx, store, e)
}

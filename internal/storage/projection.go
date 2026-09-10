package storage

import (
	"context"
	"database/sql"
)

// AddBeforeCommit appends a projection to this save's transaction. Projections
// run in registration order and the first error rolls back the complete save.
func (o *SaveOptions) AddBeforeCommit(next func(context.Context, *sql.Tx) error) {
	if next == nil {
		return
	}
	previous := o.BeforeCommit
	o.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
		if previous != nil {
			if err := previous(ctx, tx); err != nil {
				return err
			}
		}
		return next(ctx, tx)
	}
}

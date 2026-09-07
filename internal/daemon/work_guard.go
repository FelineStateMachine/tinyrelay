package daemon

import (
	"context"
	"encoding/json"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/work"
)

func (t *Tenant) guardWork(next work.Handler) work.Handler {
	return func(ctx context.Context, intent work.Intent) error {
		if intent.Kind == "job" {
			var spec replication.JobSpec
			if json.Unmarshal([]byte(intent.Payload), &spec) == nil && spec.Kind == replication.JobBackup {
				// The snapshot provider acquires exclusive admission itself.
				return next(ctx, intent)
			}
		}
		ctx, done, err := t.beginOperation(ctx)
		if err != nil {
			return err
		}
		defer done()
		return next(ctx, intent)
	}
}
